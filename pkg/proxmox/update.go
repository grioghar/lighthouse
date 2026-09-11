package proxmox

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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
	tmpl := u.TemplateDir + "/" + TemplateFile(g.Image)
	installed, err := u.Client.InstalledDigestOnNode(ctx, tmpl)
	if err != nil {
		r.Err = fmt.Errorf("installed digest: %w", err)
		return r
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

func (u *Updater) checkPackages(ctx context.Context, g Guest) Result {
	r := Result{Guest: g, Kind: ModePackages}
	if g.Status != "running" {
		r.Action = "skipped (not running)"
		return r
	}
	out, err := u.Client.ExecInGuest(ctx, g.VMID, "sh", "-lc",
		"apt-get update -qq >/dev/null 2>&1; apt-get -s dist-upgrade 2>/dev/null | grep -c '^Inst ' || true")
	if err != nil {
		r.Err = fmt.Errorf("counting upgradable packages: %w", err)
		return r
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(lastLine(out)))
	if convErr != nil {
		r.Err = fmt.Errorf("unexpected apt output %q", truncate(strings.TrimSpace(out), 120))
		return r
	}
	r.Packages = n
	r.Outdated = n > 0
	switch {
	case n == 0:
		r.Action = "up to date"
	case u.DryRun:
		r.Action = fmt.Sprintf("would upgrade %d package(s)", n)
	default:
		if _, err := u.Client.ExecInGuest(ctx, g.VMID, "sh", "-lc",
			"DEBIAN_FRONTEND=noninteractive apt-get -y -o Dpkg::Options::=--force-confold dist-upgrade"); err != nil {
			r.Err = fmt.Errorf("upgrading: %w", err)
			return r
		}
		r.Action = fmt.Sprintf("upgraded %d package(s)", n)
	}
	return r
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}
