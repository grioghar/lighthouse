package pkgprov

import (
	"context"
	"strconv"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/proxmox"
	"github.com/grioghar/lighthouse/pkg/update"
)

// Guests keeps the packages inside Proxmox LXC guests current.
//
// This covers both conventional LXCs and OCI-derived ones, which is worth
// saying because they are updated by completely different means and a homelab
// has both. An OCI guest's *image* is tracked by the ociguest provider; this
// one updates the packages running inside whatever guest exists right now.
// Both are legitimate and they are not alternatives: Proxmox squashes image
// layers into a single rootfs at creation, so an OCI guest cannot be updated
// in place from its image at all -- until someone rebuilds it, its package
// manager is the only thing that will ever patch it.
type Guests struct {
	core
	client *proxmox.Client
	// SkipOCI omits OCI-derived guests, for operators who rebuild those from
	// images and do not want packages drifting from the image in between.
	SkipOCI bool
	// IncludeStopped is off by default: pct exec cannot enter a stopped guest,
	// and starting one to patch it is a decision an operator makes, not a
	// side effect of a scheduled check.
	IncludeStopped bool
}

// NewGuests builds the LXC guest package provider.
func NewGuests(client *proxmox.Client, opts Options) (*Guests, error) {
	forced, err := opts.backend()
	if err != nil {
		return nil, err
	}
	g := &Guests{client: client}
	g.core = core{opts: opts, forced: forced, runnerA: func(t update.Target) execx.Runner {
		vmid, err := strconv.Atoi(t.Label("vmid"))
		if err != nil {
			return nil
		}
		// Composition, not a special case: "inside guest N, on a node reached
		// however the client reaches it".
		return execx.Guest{Base: client.Runner(), VMID: vmid}
	}}
	return g, nil
}

func (g *Guests) Name() string { return "lxc-packages" }

func (g *Guests) Describe() string {
	return "Distribution packages inside Proxmox LXC guests (including OCI-derived ones)."
}

// Discover lists the guests worth acting on.
func (g *Guests) Discover(ctx context.Context) ([]update.Target, error) {
	guests, err := g.client.ListGuests(ctx)
	if err != nil {
		return nil, err
	}
	var out []update.Target
	for _, gu := range guests {
		if gu.OCIManaged() && g.SkipOCI {
			continue
		}
		if gu.Status != "running" && !g.IncludeStopped {
			continue
		}
		runtime := "lxc"
		if gu.OCIManaged() {
			runtime = "oci"
		}
		labels := map[string]string{
			"vmid":   strconv.Itoa(gu.VMID),
			"status": gu.Status,
		}
		if gu.Image != "" {
			labels["image"] = gu.Image
		}
		out = append(out, update.Target{
			ID:       "ct:" + strconv.Itoa(gu.VMID),
			Name:     gu.Name,
			Runtime:  runtime,
			Location: execx.Where(g.client.Runner()),
			Labels:   labels,
		})
	}
	return out, nil
}

func (g *Guests) Check(ctx context.Context, t update.Target) update.Result {
	if t.Label("status") != "running" {
		// Reported rather than attempted: pct exec into a stopped guest fails
		// with a message that reads like a broken guest.
		return update.Result{Target: t, Outcome: update.Skipped,
			Detail: "not running"}
	}
	return g.core.check(ctx, t)
}

func (g *Guests) Apply(ctx context.Context, t update.Target) update.Result {
	return g.core.apply(ctx, t)
}
