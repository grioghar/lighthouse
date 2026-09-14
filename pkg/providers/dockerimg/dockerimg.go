// Package dockerimg exposes Lighthouse's original Docker path as an update
// provider, so one scheduler covers containers, guests and hosts alike.
//
// This is a wrapper, not a reimplementation. The container update logic --
// dependency ordering, linked containers, lifecycle hooks, rolling restarts,
// health-gated rollback -- is hard-won and lives in internal/actions. What
// this adds is the Provider shape, so that a single run can report "three
// containers, one guest and the node all need attention" instead of the
// operator running three different things and correlating the output.
//
// It is a BatchApplier because container updates are not independent of one
// another: a linked container must restart when its dependency is replaced,
// and a rolling restart is a property of the set. Applying them one at a time
// would produce a plausible-looking run that quietly breaks ordering.
package dockerimg

import (
	"context"
	"fmt"
	"strings"

	"github.com/grioghar/lighthouse/internal/actions"
	"github.com/grioghar/lighthouse/pkg/container"
	t "github.com/grioghar/lighthouse/pkg/types"
	"github.com/grioghar/lighthouse/pkg/update"
)

// Provider updates Docker containers by replacing them with newer images.
type Provider struct {
	Client container.Client
	// Params is the existing update configuration, passed through untouched so
	// that every flag the Docker path already honours keeps working.
	Params t.UpdateParams
	// Host describes where the daemon is, for the report.
	Host string
}

func (p *Provider) Name() string { return "docker-images" }

func (p *Provider) Describe() string {
	return "Docker containers whose image has a newer version (replaces the container)."
}

// Discover lists the containers the configured filter selects.
func (p *Provider) Discover(_ context.Context) ([]update.Target, error) {
	containers, err := p.Client.ListContainers(p.Params.Filter)
	if err != nil {
		return nil, err
	}
	host := p.Host
	if host == "" {
		host = "docker"
	}
	out := make([]update.Target, 0, len(containers))
	for _, c := range containers {
		out = append(out, update.Target{
			ID:       "docker:" + strings.TrimPrefix(c.Name(), "/"),
			Name:     strings.TrimPrefix(c.Name(), "/"),
			Runtime:  "docker",
			Location: host,
			Labels: map[string]string{
				"container": strings.TrimPrefix(c.Name(), "/"),
				"image":     c.ImageName(),
				"image-id":  c.SafeImageID().ShortID(),
			},
		})
	}
	return out, nil
}

// Check asks whether a newer image exists for this container.
//
// Note that this pulls the image when pulling is enabled, which is a write to
// the local image store. It does not change anything running -- the container
// is untouched until Apply -- and it is what Lighthouse's monitor-only mode has
// always done, because there is no way to know an image is stale without
// fetching its manifest.
func (p *Provider) Check(_ context.Context, target update.Target) update.Result {
	c, err := p.find(target)
	if err != nil {
		return update.Result{Target: target, Outcome: update.Skipped,
			Detail: "no longer present: " + err.Error()}
	}

	stale, newest, err := p.Client.IsContainerStale(c, p.Params)
	if err != nil {
		return update.Result{Target: target, Outcome: update.Failed, Err: err,
			Detail: "checking image: " + err.Error()}
	}
	res := update.Result{
		Target:    target,
		Installed: c.SafeImageID().ShortID(),
		Available: newest.ShortID(),
	}
	// A stopped container is still reported as outdated, but the report says
	// so: updating it starts it, which is a surprise if you stopped it on
	// purpose.
	if stale && !c.IsRunning() && !p.Params.NoRestart {
		res.Outcome = update.Outdated
		res.Detail = fmt.Sprintf("%s: newer image %s (container is stopped)",
			c.ImageName(), newest.ShortID())
		return res
	}
	if !stale {
		res.Outcome = update.UpToDate
		res.Detail = c.ImageName() + ": up to date"
		return res
	}
	res.Outcome = update.Outdated
	res.Detail = fmt.Sprintf("%s: newer image %s", c.ImageName(), newest.ShortID())
	return res
}

// Apply is never called: this provider is a BatchApplier. It satisfies the
// interface and reports honestly if the engine ever changes.
func (p *Provider) Apply(_ context.Context, target update.Target) update.Result {
	return update.Result{Target: target, Outcome: update.Failed,
		Detail: "docker containers are updated as a set; ApplyAll should have been used",
		Err:    fmt.Errorf("dockerimg: Apply called on a batch provider")}
}

// ApplyAll updates every outdated container in one pass, preserving the
// dependency ordering that makes the update correct.
func (p *Provider) ApplyAll(_ context.Context, outdated []update.Target) []update.Result {
	// Narrow the existing filter to just the containers that need updating, so
	// the ordering logic still sees a coherent set but nothing up to date gets
	// restarted for no reason.
	wanted := make(map[string]bool, len(outdated))
	for _, t := range outdated {
		wanted[t.Label("container")] = true
	}
	params := p.Params
	base := params.Filter
	params.Filter = func(c t.FilterableContainer) bool {
		if base != nil && !base(c) {
			return false
		}
		return wanted[strings.TrimPrefix(c.Name(), "/")]
	}

	report, err := actions.Update(p.Client, params)
	if err != nil {
		out := make([]update.Result, 0, len(outdated))
		for _, t := range outdated {
			out = append(out, update.Result{Target: t, Outcome: update.Failed,
				Err: err, Detail: "update run failed: " + err.Error()})
		}
		return out
	}
	return translate(report, outdated)
}

// translate maps a session report back onto the targets that were asked for.
func translate(report t.Report, outdated []update.Target) []update.Result {
	byName := map[string]update.Target{}
	for _, t := range outdated {
		byName[t.Label("container")] = t
	}
	find := func(name string) update.Target {
		name = strings.TrimPrefix(name, "/")
		if t, ok := byName[name]; ok {
			return t
		}
		return update.Target{ID: "docker:" + name, Name: name, Runtime: "docker"}
	}

	var out []update.Result
	for _, r := range report.Updated() {
		out = append(out, update.Result{Target: find(r.Name()), Outcome: update.Updated,
			Detail: fmt.Sprintf("updated to %s", r.CurrentImageID().ShortID())})
	}
	for _, r := range report.Failed() {
		out = append(out, update.Result{Target: find(r.Name()), Outcome: update.Failed,
			Detail: "update failed: " + r.Error(), Err: fmt.Errorf("%s", r.Error())})
	}
	for _, r := range report.Skipped() {
		out = append(out, update.Result{Target: find(r.Name()), Outcome: update.Skipped,
			Detail: "skipped: " + r.Error()})
	}
	// Anything the run considered fresh after the fact is up to date, which
	// happens when a pull during the update resolved to the same image.
	for _, r := range report.Fresh() {
		out = append(out, update.Result{Target: find(r.Name()), Outcome: update.UpToDate,
			Detail: "already up to date"})
	}
	return out
}

func (p *Provider) find(target update.Target) (t.Container, error) {
	name := target.Label("container")
	containers, err := p.Client.ListContainers(p.Params.Filter)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		if strings.TrimPrefix(c.Name(), "/") == name {
			return c, nil
		}
	}
	return nil, fmt.Errorf("container %q not found", name)
}
