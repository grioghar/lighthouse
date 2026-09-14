package pkgprov

import (
	"context"
	"strings"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/pkgmgr"
	"github.com/grioghar/lighthouse/pkg/update"
)

// Host keeps the machine itself current -- in practice the Proxmox node.
//
// This provider is deliberately the most conservative one in the tree, because
// the blast radius is the entire fleet. A hypervisor running 46 guests is not
// a cattle host: an upgrade that goes wrong takes everything with it, and a
// reboot takes everything down whether it goes wrong or not.
//
// So: it applies package upgrades when asked, and it never reboots. A pending
// reboot is reported loudly and left for a human to schedule. Proxmox's own
// kernel packages are called out by name, because installing one changes
// nothing at all until the node is restarted, and an operator reading
// "upgraded, up to date" would reasonably believe otherwise.
type Host struct {
	core
	runner execx.Runner
	// Node is the PVE node name when this host is one, for the report.
	Node string
	// Label overrides the target name. Empty derives one from the runner.
	Label string
}

// NewHost builds the host package provider. The runner decides which machine
// "the host" is: execx.Local for the machine Lighthouse runs on, execx.SSH for
// a Proxmox node it is merely pointed at.
func NewHost(runner execx.Runner, node string, opts Options) (*Host, error) {
	forced, err := opts.backend()
	if err != nil {
		return nil, err
	}
	h := &Host{runner: runner, Node: node}
	h.core = core{opts: opts, forced: forced, runnerA: func(update.Target) execx.Runner {
		return h.runner
	}}
	return h, nil
}

func (h *Host) Name() string { return "host-packages" }

func (h *Host) Describe() string {
	return "Distribution packages on the host itself (Proxmox node). Never reboots."
}

// Serial is true because dpkg and rpm take a machine-wide lock. Two concurrent
// upgrades of one host do not go faster; the second waits on the lock or dies.
func (h *Host) Serial() bool { return true }

// Discover returns the single host target.
func (h *Host) Discover(_ context.Context) ([]update.Target, error) {
	name := h.Label
	if name == "" {
		name = h.Node
	}
	if name == "" {
		name = execx.Where(h.runner)
	}
	return []update.Target{{
		ID:       "host:" + name,
		Name:     name,
		Runtime:  "host",
		Location: execx.Where(h.runner),
		Labels:   map[string]string{"node": h.Node},
	}}, nil
}

func (h *Host) Check(ctx context.Context, t update.Target) update.Result {
	return h.annotate(h.core.check(ctx, t))
}

func (h *Host) Apply(ctx context.Context, t update.Target) update.Result {
	return h.annotate(h.core.apply(ctx, t))
}

// annotate adds the hypervisor-specific warnings that make the difference
// between a report an operator can act on and one that quietly misleads.
func (h *Host) annotate(r update.Result) update.Result {
	if kernels := kernelPackages(r.Packages); len(kernels) > 0 {
		r.Detail += "; includes kernel (" + strings.Join(kernels, ", ") +
			") -- no effect until the node is rebooted"
		// A kernel upgrade always implies a reboot, whether or not the distro
		// wrote its marker file yet. Leaving this Unknown would let a run that
		// just installed a new kernel report nothing about restarting.
		if r.Outcome == update.Updated && r.Reboot == pkgmgr.Unknown {
			r.Reboot = pkgmgr.Yes
		}
	}
	if r.Reboot == pkgmgr.Yes {
		r.Detail += "; REBOOT REQUIRED -- not performed, schedule it " +
			"(guests must be migrated or stopped first)"
	}
	return r
}

// kernelPackages picks kernel packages out of a pending set. The Proxmox
// names are what matter here: pve-kernel-* on older releases, proxmox-kernel-*
// since 8.x, plus the stock Debian names for a non-PVE host.
func kernelPackages(pkgs []string) []string {
	var out []string
	for _, p := range pkgs {
		lower := strings.ToLower(p)
		switch {
		case strings.HasPrefix(lower, "proxmox-kernel-"),
			strings.HasPrefix(lower, "pve-kernel-"),
			strings.HasPrefix(lower, "linux-image-"),
			lower == "linux" || lower == "kernel" || lower == "kernel-core":
			out = append(out, p)
		}
	}
	return out
}
