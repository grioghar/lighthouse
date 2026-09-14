// Package pkgprov turns "run the distribution's package manager" into update
// providers for every place Lighthouse can reach one.
//
// The Proxmox node, an LXC guest, an OCI-derived guest and a Docker container
// differ only in how a command gets inside them -- which is exactly what an
// execx.Runner abstracts. So the check-and-upgrade logic lives here once, and
// the three providers below differ only in how they discover targets and which
// Runner they build for each one.
package pkgprov

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/pkgmgr"
	"github.com/grioghar/lighthouse/pkg/update"
)

// maxListed caps how many package names reach a report. A Debian guest that
// has not been touched in a year has several hundred pending, and printing all
// of them buries every other target in the run.
const maxListed = 8

// Options are the settings shared by every package provider.
type Options struct {
	// Manager forces a specific package manager instead of detecting one.
	// Detection is a round trip per target, and an operator running a
	// homogeneous fleet can skip it.
	Manager string
	// MaxListed caps package names in the report. Zero means maxListed.
	MaxListed int
}

func (o Options) backend() (*pkgmgr.Backend, error) {
	if o.Manager == "" {
		return nil, nil
	}
	b, ok := pkgmgr.Lookup(o.Manager)
	if !ok {
		names := make([]string, 0, len(pkgmgr.Backends))
		for _, x := range pkgmgr.Backends {
			names = append(names, x.Name)
		}
		return nil, fmt.Errorf("unknown package manager %q; known: %s",
			o.Manager, strings.Join(names, ", "))
	}
	return b, nil
}

func (o Options) cap() int {
	if o.MaxListed > 0 {
		return o.MaxListed
	}
	return maxListed
}

// core is the shared check/apply logic. Everything specific to a kind of
// target is in the runner it is handed and the targets it is given.
type core struct {
	opts    Options
	forced  *pkgmgr.Backend
	runnerA func(update.Target) execx.Runner
}

// check reports pending upgrades without installing anything.
func (c core) check(ctx context.Context, t update.Target) update.Result {
	r := c.runnerA(t)
	if r == nil {
		return update.Result{Target: t, Outcome: update.Skipped,
			Detail: "no way to run commands in this target"}
	}

	backend := c.forced
	if backend == nil {
		var err error
		backend, err = pkgmgr.Detect(ctx, r)
		if err != nil {
			return c.detectFailure(t, err)
		}
	}

	st, err := backend.Check(ctx, r)
	if err != nil {
		var nested *execx.NestedError
		if errors.As(err, &nested) {
			return update.Result{Target: t, Outcome: update.Skipped,
				Detail: "unreachable: " + nested.Detail}
		}
		return update.Result{Target: t, Outcome: update.Failed, Err: err,
			Detail: "checking packages: " + err.Error()}
	}

	res := update.Result{
		Target:   t,
		Pending:  st.Count(),
		Packages: st.Names(),
		Reboot:   st.Reboot,
	}
	if len(res.Packages) > c.opts.cap() {
		res.Packages = res.Packages[:c.opts.cap()]
	}
	if st.Count() == 0 {
		res.Outcome = update.UpToDate
		res.Detail = fmt.Sprintf("%s: up to date", st.Manager)
		return res
	}
	res.Outcome = update.Outdated
	res.Detail = fmt.Sprintf("%s: %d pending (%s)",
		st.Manager, st.Count(), st.Summary(c.opts.cap()))
	return res
}

// detectFailure distinguishes "this target has no package manager", which is
// normal and must not be reported as broken, from a target we could not reach.
func (c core) detectFailure(t update.Target, err error) update.Result {
	var nested *execx.NestedError
	switch {
	case errors.Is(err, pkgmgr.ErrNoManager):
		// A distroless or scratch image has none by design. So does a guest
		// built from an image whose whole point is a single static binary.
		return update.Result{Target: t, Outcome: update.Skipped,
			Detail: "no supported package manager"}
	case errors.As(err, &nested):
		return update.Result{Target: t, Outcome: update.Skipped,
			Detail: "unreachable: " + nested.Detail}
	default:
		return update.Result{Target: t, Outcome: update.Failed, Err: err,
			Detail: "detecting package manager: " + err.Error()}
	}
}

// apply installs every pending upgrade.
func (c core) apply(ctx context.Context, t update.Target) update.Result {
	r := c.runnerA(t)
	if r == nil {
		return update.Result{Target: t, Outcome: update.Skipped,
			Detail: "no way to run commands in this target"}
	}
	backend := c.forced
	if backend == nil {
		var err error
		backend, err = pkgmgr.Detect(ctx, r)
		if err != nil {
			return c.detectFailure(t, err)
		}
	}
	if err := backend.Upgrade(ctx, r); err != nil {
		return update.Result{Target: t, Outcome: update.Failed, Err: err,
			Detail: "upgrading: " + err.Error()}
	}

	// Re-check afterwards. An upgrade that "succeeded" while leaving packages
	// held back is the normal apt outcome for a phased or held package, and
	// reporting it as done would hide that. Re-checking is also how the reboot
	// flag gets its post-upgrade value, which is the one that matters.
	st, err := backend.Check(ctx, r)
	if err != nil {
		return update.Result{Target: t, Outcome: update.Updated,
			Detail: "upgraded (post-check failed: " + err.Error() + ")"}
	}
	res := update.Result{Target: t, Outcome: update.Updated, Reboot: st.Reboot}
	if st.Count() > 0 {
		res.Pending = st.Count()
		res.Packages = st.Names()
		if len(res.Packages) > c.opts.cap() {
			res.Packages = res.Packages[:c.opts.cap()]
		}
		res.Detail = fmt.Sprintf("upgraded; %d still held back (%s)",
			st.Count(), st.Summary(c.opts.cap()))
		return res
	}
	res.Detail = "upgraded, now up to date"
	return res
}
