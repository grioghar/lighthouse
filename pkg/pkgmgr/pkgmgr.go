// Package pkgmgr keeps "what does this distribution call an upgrade" in one
// table instead of spread across the update backends.
//
// An image digest says nothing about a plain Debian LXC, and apt says nothing
// about an OCI-derived one. Lighthouse needs both, and the package half has to
// work against whatever the guest happens to be -- a homelab accumulates
// Debian, Ubuntu, Alpine and the occasional Rocky without anyone planning it.
//
// Each distribution is one Backend value: the probe that finds it, the
// commands that list and apply upgrades, and a parser for its output format.
// Adding a distribution is adding a table entry, not touching any caller.
//
// Everything here runs through an execx.Runner, so exactly the same code
// upgrades the machine Lighthouse is on, a Proxmox node over ssh, the inside
// of an LXC guest, and the inside of a Docker container. That is the point:
// the interesting variation is the distribution, not the location.
package pkgmgr

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/grioghar/lighthouse/pkg/execx"
)

// Package is one upgradable package.
type Package struct {
	Name      string
	Installed string // may be empty when the manager does not report it
	Candidate string
}

func (p Package) String() string {
	switch {
	case p.Installed != "" && p.Candidate != "":
		return fmt.Sprintf("%s %s->%s", p.Name, p.Installed, p.Candidate)
	case p.Candidate != "":
		return p.Name + " " + p.Candidate
	default:
		return p.Name
	}
}

// Status is what a check found on one target.
type Status struct {
	Manager   string
	Pending   []Package
	Reboot    Tristate
	Refreshed bool
}

// Count returns the number of pending packages.
func (s Status) Count() int { return len(s.Pending) }

// Names returns pending package names, sorted, for a stable report.
func (s Status) Names() []string {
	out := make([]string, 0, len(s.Pending))
	for _, p := range s.Pending {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Summary renders the pending set compactly, capped so a 400-package Debian
// guest does not flood a notification.
func (s Status) Summary(max int) string {
	names := s.Names()
	if len(names) == 0 {
		return "up to date"
	}
	if max > 0 && len(names) > max {
		return fmt.Sprintf("%s and %d more", strings.Join(names[:max], ", "), len(names)-max)
	}
	return strings.Join(names, ", ")
}

// Tristate distinguishes "no reboot needed" from "this distribution has no way
// to tell me". Reporting unknown as false would quietly claim a hypervisor is
// safe to leave running when nothing actually checked.
type Tristate int

const (
	Unknown Tristate = iota
	No
	Yes
)

func (t Tristate) String() string {
	switch t {
	case Yes:
		return "yes"
	case No:
		return "no"
	default:
		return "unknown"
	}
}

// Backend describes one distribution's package manager.
//
// The command fields are shell scripts rather than argv because that is what
// these operations honestly are -- pipelines of the distro's own tools -- and
// because a single string crosses an ssh hop or a pct exec without needing
// every layer to preserve argument boundaries.
type Backend struct {
	// Name is the manager's own name, and the value users pass to override
	// detection.
	Name string
	// Binary is what detection looks for on PATH.
	Binary string
	// RefreshCmd updates the package index. Empty means the manager has no
	// separate refresh step, or folds it into List.
	RefreshCmd string
	// RefreshWarnings are substrings that mean the refresh partially failed
	// even though it exited zero.
	//
	// This is not defensive programming, it is apt: `apt-get update` exits 0
	// when a repository is unreachable and reports it only as a W: line. Since
	// a stale index reports "up to date", exit-code-only detection would turn
	// a broken repo into a confident wrong answer -- and apt is the manager
	// most of a Proxmox fleet runs. apk and zypper exit non-zero and need
	// nothing here.
	RefreshWarnings []string
	// List prints the pending upgrades in this manager's native format.
	ListCmd string
	// ListOKCodes are exit codes from List that mean success. dnf and zypper
	// signal "updates are available" with a non-zero exit, which is data, not
	// failure -- see execx's package comment.
	ListOKCodes []int
	// Upgrade applies every pending upgrade, non-interactively.
	UpgradeCmd string
	// RebootCheck prints yes, no, or anything else for unknown.
	RebootCmd string
	// Parse turns List output into packages.
	Parse func(stdout string) []Package
}

// Backends is the registry, in detection order. The order only matters on a
// system carrying more than one manager, where the distribution's native one
// should win: a Debian host with dnf installed as a tool is still apt's.
var Backends = []*Backend{apt, dnf, yum, apk, zypper, pacman}

// Lookup returns a backend by name.
func Lookup(name string) (*Backend, bool) {
	for _, b := range Backends {
		if strings.EqualFold(b.Name, name) {
			return b, true
		}
	}
	return nil, false
}

// ErrNoManager means the target has no package manager this package knows.
// It is not a failure of the target -- a distroless or scratch container
// legitimately has none, and the caller should skip rather than report broken.
var ErrNoManager = fmt.Errorf("pkgmgr: no supported package manager found")

// Detect finds which backend a target uses.
//
// One round trip, not one per candidate: over ssh into a guest, probing six
// managers separately is six connections' worth of latency per guest, and a
// node with 46 guests makes that the dominant cost of a run.
func Detect(ctx context.Context, r execx.Runner) (*Backend, error) {
	var sb strings.Builder
	for _, b := range Backends {
		fmt.Fprintf(&sb, "command -v %s >/dev/null 2>&1 && { echo %s; exit 0; }; ", b.Binary, b.Name)
	}
	sb.WriteString("exit 0")

	res, err := execx.Script(ctx, r, sb.String())
	if err != nil {
		return nil, err
	}
	name := res.Out()
	if name == "" {
		return nil, ErrNoManager
	}
	b, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("pkgmgr: probe returned unknown manager %q", name)
	}
	return b, nil
}

// Check reports what is pending on a target without changing anything
// installed. Refresh does update the package index, which is a write, but not
// one that changes installed software; a check that skipped it would report
// stale results and is not worth having.
func (b *Backend) Check(ctx context.Context, r execx.Runner) (Status, error) {
	st := Status{Manager: b.Name}

	if b.RefreshCmd != "" {
		// A failed refresh is not fatal. A guest with one unreachable third-
		// party repo still has a usable index for everything else, and
		// refusing to report anything because of it is worse than reporting
		// against a slightly stale index. But the caller has to be told, or
		// it will present a stale "up to date" as a confident answer.
		res, err := execx.Script(ctx, r, b.RefreshCmd)
		st.Refreshed = err == nil && res.OK() && !b.refreshWarned(res)
	}

	res, err := execx.Script(ctx, r, b.ListCmd)
	if err != nil {
		return st, err
	}
	if !b.listOK(res.Code) {
		return st, fmt.Errorf("pkgmgr/%s: listing upgrades: exit %d: %s",
			b.Name, res.Code, truncate(res.Err(), 200))
	}
	st.Pending = b.Parse(res.Stdout)
	st.Reboot = b.reboot(ctx, r)
	return st, nil
}

// refreshWarned reports whether a zero-exit refresh actually failed in part.
func (b *Backend) refreshWarned(res execx.Result) bool {
	if len(b.RefreshWarnings) == 0 {
		return false
	}
	// Both streams: apt writes its W: lines to stderr, but that is a detail
	// of apt rather than a rule, and matching only one stream is how this
	// silently stops working.
	combined := res.Stdout + "\n" + res.Stderr
	for _, w := range b.RefreshWarnings {
		if strings.Contains(combined, w) {
			return true
		}
	}
	return false
}

func (b *Backend) listOK(code int) bool {
	if code == 0 {
		return true
	}
	for _, c := range b.ListOKCodes {
		if code == c {
			return true
		}
	}
	return false
}

func (b *Backend) reboot(ctx context.Context, r execx.Runner) Tristate {
	if b.RebootCmd == "" {
		return Unknown
	}
	res, err := execx.Script(ctx, r, b.RebootCmd)
	if err != nil {
		return Unknown
	}
	switch strings.TrimSpace(strings.ToLower(res.Out())) {
	case "yes":
		return Yes
	case "no":
		return No
	default:
		return Unknown
	}
}

// Upgrade applies every pending upgrade.
func (b *Backend) Upgrade(ctx context.Context, r execx.Runner) error {
	res, err := execx.Script(ctx, r, b.UpgradeCmd)
	if err != nil {
		return err
	}
	if !res.OK() {
		return fmt.Errorf("pkgmgr/%s: upgrading: exit %d: %s",
			b.Name, res.Code, truncate(res.Err(), 400))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
