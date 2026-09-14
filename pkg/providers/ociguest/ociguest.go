// Package ociguest tracks Proxmox LXC guests that were created from an OCI
// image, by comparing the manifest digest installed on the node against the
// one the registry currently serves.
//
// The asymmetry with Docker is the whole story here. A Docker container is
// replaced by pulling a newer image and recreating it, and Lighthouse has done
// that for years. Proxmox squashes every layer into one rootfs when it creates
// the guest, so there is no image to swap and no in-place update: the guest
// *is* the unpacked image, plus whatever has happened to it since.
//
// So this provider detects drift and pulls, and stops there. Applying means
// rebuilding the guest, which means reproducing its network, mountpoints,
// resources and lifecycle -- and getting any of that subtly wrong loses data
// silently. That belongs behind a per-guest decision, not an unattended run.
package ociguest

import (
	"context"
	"fmt"
	"strconv"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/proxmox"
	"github.com/grioghar/lighthouse/pkg/update"
)

// Provider compares installed OCI digests against their registries.
type Provider struct {
	Client   *proxmox.Client
	Resolver proxmox.DigestResolver
	// TemplateDir is where oci-registry-pull writes. Note this is the template
	// cache, not the "import" content directory the storage config implies.
	TemplateDir string
	Storage     string
	// Pull enables fetching a newer image for an outdated guest. Even with it
	// on, the guest is not rebuilt -- see the package comment.
	Pull bool
}

func (p *Provider) Name() string { return "oci-images" }

func (p *Provider) Describe() string {
	return "OCI image drift for Proxmox guests (detect and pull; rebuild stays manual)."
}

// Discover lists guests carrying a recorded image reference.
//
// Only tagged guests appear, because Proxmox stores no link at all between a
// guest and the image it came from, and the template filename keeps only the
// last path element and tag -- docker.io/library/alpine:3.20 becomes
// alpine_3.20.tar, losing the registry and namespace irrecoverably. The
// reference has to have been recorded by us, which is what `lighthouse proxmox
// --tag-guest` does.
func (p *Provider) Discover(ctx context.Context) ([]update.Target, error) {
	guests, err := p.Client.ListGuests(ctx)
	if err != nil {
		return nil, err
	}
	var out []update.Target
	for _, g := range guests {
		if !g.OCIManaged() {
			continue
		}
		out = append(out, update.Target{
			ID:       "ct:" + strconv.Itoa(g.VMID),
			Name:     g.Name,
			Runtime:  "oci",
			Location: execx.Where(p.Client.Runner()),
			Labels: map[string]string{
				"vmid":   strconv.Itoa(g.VMID),
				"image":  g.Image,
				"digest": g.Digest,
				"status": g.Status,
			},
		})
	}
	return out, nil
}

// Check compares the installed digest to the registry's. It never touches the
// guest -- a stopped guest is checked exactly like a running one, because the
// digest lives on the node, not inside the container.
func (p *Provider) Check(ctx context.Context, t update.Target) update.Result {
	res := update.Result{Target: t}
	image := t.Label("image")
	if image == "" {
		return update.Result{Target: t, Outcome: update.Skipped,
			Detail: "no recorded image reference"}
	}

	installed := t.Label("digest")
	if installed == "" {
		// Nothing was recorded, so fall back to reading the template. That
		// only works while the template is still on the node and still named
		// after the current tag; templates are large and routinely pruned, so
		// this is a fallback, not the design.
		var err error
		installed, err = p.Client.InstalledDigestOnNode(ctx,
			p.TemplateDir+"/"+proxmox.TemplateFile(image))
		if err != nil {
			return update.Result{Target: t, Outcome: update.Failed, Err: err,
				Detail: "no recorded digest and template unreadable; re-tag the guest to record one"}
		}
	}
	res.Installed = installed

	if p.Resolver == nil {
		return update.Result{Target: t, Outcome: update.Failed,
			Detail: "no registry resolver configured",
			Err:    fmt.Errorf("ociguest: no digest resolver")}
	}
	available, err := p.Resolver.Digest(ctx, image)
	if err != nil {
		return update.Result{Target: t, Outcome: update.Failed, Err: err,
			Detail: "registry lookup for " + image + ": " + err.Error()}
	}
	res.Available = available

	if installed == available {
		res.Outcome = update.UpToDate
		res.Detail = image + ": up to date"
		return res
	}
	res.Outcome = update.Outdated
	res.Detail = fmt.Sprintf("%s: registry has %s, guest has %s",
		image, short(available), short(installed))
	return res
}

// Apply pulls the newer image. It deliberately does not rebuild the guest.
func (p *Provider) Apply(ctx context.Context, t update.Target) update.Result {
	image := t.Label("image")
	if !p.Pull {
		return update.Result{Target: t, Outcome: update.Outdated,
			Detail: image + ": newer image available; pulling is disabled"}
	}
	upid, err := p.Client.PullOCI(ctx, p.Storage, image)
	if err != nil {
		return update.Result{Target: t, Outcome: update.Failed, Err: err,
			Detail: "pull failed: " + err.Error()}
	}
	// oci-registry-pull returns a UPID and continues in the background.
	// Skipping the wait is how you end up looking at an empty template store
	// and concluding the pull failed.
	if err := p.Client.WaitTask(ctx, upid, 0); err != nil {
		return update.Result{Target: t, Outcome: update.Failed, Err: err,
			Detail: "pull task failed: " + err.Error()}
	}
	return update.Result{
		Target:  t,
		Outcome: update.Outdated,
		Detail: fmt.Sprintf("pulled %s; CT %s still needs a manual recreate "+
			"(Proxmox squashes layers, so no in-place swap exists)",
			image, t.Label("vmid")),
	}
}

func short(digest string) string {
	const n = 19 // "sha256:" plus 12 hex, the length docker shows
	if len(digest) <= n {
		return digest
	}
	return digest[:n]
}
