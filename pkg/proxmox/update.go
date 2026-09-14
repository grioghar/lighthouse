package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/pkgmgr"
)

// Mode selects which class of guest a run acts on. They are independent and
// may be combined; none of them is implied by another.
type Mode string

const (
	// ModeOCI compares the installed manifest digest of OCI-derived guests
	// against the registry. Detection only -- it never modifies a guest.
	ModeOCI Mode = "oci"
	// ModeRecreate additionally rebuilds an out-of-date OCI guest. Destructive:
	// Proxmox squashes layers at creation, so there is no in-place swap and the
	// guest's rootfs is replaced.
	ModeRecreate Mode = "recreate"
	// ModePackages runs the distribution package manager inside conventional
	// guests. Unrelated to image digests; this is the only thing that keeps a
	// plain Debian LXC current.
	ModePackages Mode = "packages"
)

// Modes is a set of enabled modes.
type Modes map[Mode]bool

// ParseModes reads a comma-separated mode list.
func ParseModes(s string) (Modes, error) {
	m := Modes{}
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" || part == "off" || part == "none" {
			continue
		}
		switch Mode(part) {
		case ModeOCI, ModeRecreate, ModePackages:
			m[Mode(part)] = true
		default:
			return nil, fmt.Errorf("proxmox: unknown mode %q (want oci, recreate, packages)", part)
		}
	}
	// Recreating implies detecting: you cannot decide what to rebuild without
	// first comparing digests.
	if m[ModeRecreate] {
		m[ModeOCI] = true
	}
	return m, nil
}

func (m Modes) Enabled(x Mode) bool { return m[x] }

// DigestResolver returns the manifest digest a reference currently resolves to
// in its registry. It is an interface so this package stays free of registry
// and docker types, and so tests need no network.
type DigestResolver interface {
	Digest(ctx context.Context, reference string) (string, error)
}

// Result is what a run found for one guest.
type Result struct {
	Guest     Guest
	Kind      Mode
	Installed string // OCI: manifest digest currently installed
	Available string // OCI: manifest digest in the registry
	Outdated  bool
	Packages  int    // packages: count of upgradable packages
	Manager   string // packages: which package manager was detected
	Reboot    pkgmgr.Tristate
	Action    string // what was done, or would be under --dry-run
	Err       error
}

// Updater checks and (optionally) updates Proxmox guests.
type Updater struct {
	Client   *Client
	Modes    Modes
	Resolver DigestResolver
	// TemplateDir is where oci-registry-pull writes. Note this is the template
	// cache, NOT the "import" content directory that the storage config implies.
	TemplateDir string
	Storage     string
	// DryRun reports what would change without changing it. The recreate path
	// defaults to this; it is destructive and unattended rebuilds of a stateful
	// guest are rarely what someone wants the first time.
	DryRun bool
}

// Check inspects every guest and returns one Result per guest acted on.
func (u *Updater) Check(ctx context.Context) ([]Result, error) {
	guests, err := u.Client.ListGuests(ctx)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, g := range guests {
		switch {
		case g.OCIManaged() && u.Modes.Enabled(ModeOCI):
			out = append(out, u.checkOCI(ctx, g))
		case !g.OCIManaged() && u.Modes.Enabled(ModePackages):
			out = append(out, u.checkPackages(ctx, g))
		}
	}
	return out, nil
}

func (u *Updater) checkOCI(ctx context.Context, g Guest) Result {
	r := Result{Guest: g, Kind: ModeOCI}
	installed := g.Digest
	if installed == "" {
		// No digest was recorded, so fall back to the template. This only
		// works while the template is still present and still named after the
		// current tag; re-tag the guest to record the digest properly.
		tmpl := u.TemplateDir + "/" + TemplateFile(g.Image)
		var err error
		if installed, err = u.Client.InstalledDigestOnNode(ctx, tmpl); err != nil {
			r.Err = fmt.Errorf("no recorded digest and template unreadable: %w", err)
			return r
		}
	}
	r.Installed = installed

	if u.Resolver == nil {
		r.Err = fmt.Errorf("no digest resolver configured")
		return r
	}
	available, err := u.Resolver.Digest(ctx, g.Image)
	if err != nil {
		r.Err = fmt.Errorf("registry digest for %s: %w", g.Image, err)
		return r
	}
	r.Available = available
	r.Outdated = installed != available
	if !r.Outdated {
		r.Action = "up to date"
		return r
	}
	if !u.Modes.Enabled(ModeRecreate) {
		r.Action = "outdated (recreate not enabled)"
		return r
	}
	r.Action = u.recreate(ctx, g)
	return r
}

// recreate rebuilds an OCI guest from a freshly pulled image.
//
// Deliberately conservative: it pulls and reports, but stops short of
// destroying the guest unless explicitly allowed, and never runs unattended by
// default. Replacing the rootfs discards anything not on a separate
// mountpoint, so the caller has to have decided that is acceptable.
func (u *Updater) recreate(ctx context.Context, g Guest) string {
	if u.DryRun {
		return fmt.Sprintf("would pull %s and recreate CT %d", g.Image, g.VMID)
	}
	upid, err := u.Client.PullOCI(ctx, u.Storage, g.Image)
	if err != nil {
		return "pull failed: " + err.Error()
	}
	if err := u.Client.WaitTask(ctx, upid, 0); err != nil {
		return "pull task failed: " + err.Error()
	}
	// The rebuild itself is intentionally not automated here. Recreating a
	// guest means reproducing its network, mountpoints, resources and
	// lifecycle, and getting any of that subtly wrong silently loses data.
	// Pulling is the safe half; the swap belongs behind an explicit,
	// per-guest decision.
	return fmt.Sprintf("pulled %s; CT %d needs a manual recreate "+
		"(layers are squashed, so no in-place swap exists)", g.Image, g.VMID)
}

// checkPackages runs the guest's own package manager.
//
// The distribution-specific half of this lives in pkg/pkgmgr, so a guest that
// is Alpine or Rocky rather than Debian is handled by a table entry instead of
// by this function knowing about apt. Composing execx.Guest onto the client's
// runner is what puts the commands inside the guest, whether the node itself
// is local or reached over ssh.
func (u *Updater) checkPackages(ctx context.Context, g Guest) Result {
	r := Result{Guest: g, Kind: ModePackages}
	if g.Status != "running" {
		// pct exec cannot enter a stopped guest, and starting one to patch it
		// is an operator's decision, not a side effect of a scheduled check.
		r.Action = "skipped (not running)"
		return r
	}

	inside := execx.Guest{Base: u.Client.Runner(), VMID: g.VMID}
	backend, err := pkgmgr.Detect(ctx, inside)
	if err != nil {
		if errors.Is(err, pkgmgr.ErrNoManager) {
			// Legitimate for a distroless or single-binary guest, so it is a
			// skip rather than an error.
			r.Action = "skipped (no supported package manager)"
			return r
		}
		r.Err = fmt.Errorf("detecting package manager: %w", err)
		return r
	}

	st, err := backend.Check(ctx, inside)
	if err != nil {
		r.Err = fmt.Errorf("counting upgradable packages: %w", err)
		return r
	}
	r.Manager = st.Manager
	r.Packages = st.Count()
	r.Reboot = st.Reboot
	r.Outdated = st.Count() > 0

	switch {
	case st.Count() == 0:
		r.Action = st.Manager + ": up to date"
	case u.DryRun:
		r.Action = fmt.Sprintf("%s: would upgrade %d package(s): %s",
			st.Manager, st.Count(), st.Summary(6))
	default:
		if err := backend.Upgrade(ctx, inside); err != nil {
			r.Err = fmt.Errorf("upgrading: %w", err)
			return r
		}
		r.Action = fmt.Sprintf("%s: upgraded %d package(s)", st.Manager, st.Count())
		if st.Reboot == pkgmgr.Yes {
			r.Action += "; reboot required"
		}
	}
	return r
}
