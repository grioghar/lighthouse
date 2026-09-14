package pkgprov

import (
	"context"
	"strings"

	"github.com/grioghar/lighthouse/pkg/execx"
	"github.com/grioghar/lighthouse/pkg/update"
)

// Containers upgrades packages *inside* running Docker containers.
//
// This is off by default and should stay that way for anything built from a
// maintained image, because it fights the model: the container's filesystem is
// ephemeral, so every upgrade is silently discarded the next time the image is
// pulled and the container recreated. For an image-managed container the
// correct fix is a newer image, which is what Lighthouse's Docker path already
// does.
//
// It exists for the cases where that is not true and pretending otherwise
// helps nobody: a container whose image is abandoned upstream, one built
// locally and not rebuilt on a schedule, or a long-lived container holding
// state that nobody is going to recreate this quarter. Patching those in place
// is worse than rebuilding and much better than leaving them.
type Containers struct {
	core
	base execx.Runner
	// User runs the package manager as a specific user. Images whose default
	// user is unprivileged need root here, and the failure without it is an
	// unhelpful permission error from the package manager.
	User string
	// Names limits the provider to specific containers. Empty means every
	// running container, which is rarely what anyone wants -- see above.
	Names []string
}

// NewContainers builds the in-container package provider. base must reach a
// Docker CLI that can talk to the daemon.
func NewContainers(base execx.Runner, opts Options) (*Containers, error) {
	forced, err := opts.backend()
	if err != nil {
		return nil, err
	}
	c := &Containers{base: base}
	c.core = core{opts: opts, forced: forced, runnerA: func(t update.Target) execx.Runner {
		return execx.Container{Base: c.base, ID: t.Label("container"), User: c.User}
	}}
	return c, nil
}

func (c *Containers) Name() string { return "docker-packages" }

func (c *Containers) Describe() string {
	return "Distribution packages inside running Docker containers (opt-in; prefer image updates)."
}

// Discover lists running containers.
func (c *Containers) Discover(ctx context.Context) ([]update.Target, error) {
	if len(c.Names) > 0 {
		out := make([]update.Target, 0, len(c.Names))
		for _, n := range c.Names {
			out = append(out, c.target(n, ""))
		}
		return out, nil
	}
	// A tab-separated format rather than the default table: container names
	// and image references both contain spaces in no sane case, but image
	// references contain colons and slashes that a space-split would keep,
	// and the header line would otherwise need stripping.
	out, err := execx.Output(ctx, c.base, "docker", "ps",
		"--format", "{{.Names}}\t{{.Image}}")
	if err != nil {
		return nil, err
	}
	var targets []update.Target
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		image := ""
		if len(parts) > 1 {
			image = parts[1]
		}
		targets = append(targets, c.target(parts[0], image))
	}
	return targets, nil
}

func (c *Containers) target(name, image string) update.Target {
	labels := map[string]string{"container": name}
	if image != "" {
		labels["image"] = image
	}
	return update.Target{
		ID:       "docker:" + name,
		Name:     name,
		Runtime:  "docker",
		Location: execx.Where(c.base),
		Labels:   labels,
	}
}

func (c *Containers) Check(ctx context.Context, t update.Target) update.Result {
	r := c.core.check(ctx, t)
	// Say it on every actionable line rather than once in the docs, because
	// this is the detail that makes the result misleading if forgotten.
	if r.Outcome == update.Outdated || r.Outcome == update.Updated {
		r.Detail += "; note: in-container changes are lost when the container " +
			"is recreated from its image"
	}
	return r
}

func (c *Containers) Apply(ctx context.Context, t update.Target) update.Result {
	return c.core.apply(ctx, t)
}
