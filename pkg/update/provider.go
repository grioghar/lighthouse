// Package update is the modular core: one interface that every class of
// updatable thing implements, and one engine that drives them.
//
// Lighthouse started as a Docker image updater, where "update" had exactly one
// meaning. It now has at least four, and they share almost nothing:
//
//	Docker container   replace it with one built from a newer image
//	OCI-derived LXC    compare the installed manifest digest to the registry
//	conventional LXC   run the distribution's package manager inside it
//	Proxmox node       run apt on the hypervisor itself, and never reboot it
//
// The temptation is a switch statement somewhere central. That is what this
// package exists to avoid: each of those is a Provider, registered by name,
// and the engine below knows only the interface. Adding a fifth -- a VM, a
// firmware image, a Kubernetes node -- is a new file, not an edit to a
// dispatcher.
//
// The split between Check and Apply is deliberate and load-bearing. Check must
// never change installed software, so a scheduled run against a production
// hypervisor is safe by construction, and dry-run is not a flag each provider
// has to remember to honour -- it is simply Apply never being called.
package update

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grioghar/lighthouse/pkg/pkgmgr"
)

// Target is one thing a provider can act on.
type Target struct {
	// ID is stable across runs and unique within a provider, e.g. "ct:3117".
	ID string
	// Name is what a human calls it.
	Name string
	// Runtime is the kind of thing this is: docker, lxc, oci, host. It is
	// reported rather than dispatched on -- the provider already knows.
	Runtime string
	// Location describes where commands for this target land, for the report.
	Location string
	// Labels carries provider-specific detail through to the report without
	// the engine needing to understand any of it.
	Labels map[string]string
}

func (t Target) String() string {
	if t.Name == "" {
		return t.ID
	}
	return fmt.Sprintf("%s (%s)", t.Name, t.ID)
}

// Label reads a label, returning empty when absent.
func (t Target) Label(k string) string {
	if t.Labels == nil {
		return ""
	}
	return t.Labels[k]
}

// Outcome is what a check or an apply concluded.
type Outcome string

const (
	// UpToDate: nothing to do.
	UpToDate Outcome = "up-to-date"
	// Outdated: an update exists and was not applied, either because this was
	// a check or because applying is disabled.
	Outdated Outcome = "outdated"
	// Updated: an update was applied successfully.
	Updated Outcome = "updated"
	// Skipped: this target was not eligible -- stopped, no package manager,
	// excluded by a filter. Not a failure.
	Skipped Outcome = "skipped"
	// Failed: the check or the update itself went wrong.
	Failed Outcome = "failed"
)

// Actionable reports whether this outcome is one an operator needs to see.
func (o Outcome) Actionable() bool { return o == Outdated || o == Failed }

// Result is what one provider concluded about one target.
type Result struct {
	Provider string
	Target   Target
	Outcome  Outcome
	// Detail is the human-readable summary: what was found, or what was done.
	Detail string
	// Pending is the number of package upgrades available, for package
	// providers. Zero for image providers, which have no such notion.
	Pending int
	// Packages names what is pending, capped by the provider.
	Packages []string
	// Reboot reports whether the target needs restarting. Unknown is a real
	// answer and must not be rendered as "no".
	Reboot pkgmgr.Tristate
	// Installed and Available are manifest digests, for image providers.
	Installed string
	Available string
	Err       error
	Duration  time.Duration
}

// Provider discovers and updates one class of target.
//
// Implementations must guarantee that Check never changes installed software.
// Refreshing a package index is permitted -- without it a check reports stale
// results and is not worth having -- but nothing may be installed, removed,
// restarted or recreated.
type Provider interface {
	// Name identifies the provider on the command line and in reports.
	Name() string
	// Describe is a one-line summary for `lighthouse providers`.
	Describe() string
	// Discover lists the targets this provider can act on right now.
	Discover(ctx context.Context) ([]Target, error)
	// Check reports a target's state without changing it.
	Check(ctx context.Context, t Target) Result
	// Apply performs the update. The engine calls it only for targets whose
	// Check returned Outdated, and only when applying is enabled.
	Apply(ctx context.Context, t Target) Result
}

// Registry holds the available providers.
//
// Providers register at construction rather than init() so that a provider
// requiring configuration -- an ssh host, a node name -- can be built with it
// rather than reaching for globals.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

// Add registers a provider, replacing any earlier one with the same name.
func (r *Registry) Add(p Provider) *Registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[p.Name()]; !exists {
		r.order = append(r.order, p.Name())
	}
	r.providers[p.Name()] = p
	return r
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// All returns the registered providers in registration order, so reports and
// logs are stable between runs.
func (r *Registry) All() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Provider, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.providers[n])
	}
	return out
}

// Names returns the registered provider names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]string(nil), r.order...)
	return out
}

// Select returns the providers named in the list, erroring on any unknown
// name. An empty or "all" selection returns everything registered.
func (r *Registry) Select(names []string) ([]Provider, error) {
	if len(names) == 0 {
		return r.All(), nil
	}
	var out []Provider
	var unknown []string
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if n == "all" {
			return r.All(), nil
		}
		p, ok := r.Get(n)
		if !ok {
			unknown = append(unknown, n)
			continue
		}
		out = append(out, p)
	}
	if len(unknown) > 0 {
		avail := r.Names()
		sort.Strings(avail)
		return nil, fmt.Errorf("update: unknown provider(s) %s; available: %s",
			strings.Join(unknown, ", "), strings.Join(avail, ", "))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("update: no providers selected")
	}
	return out, nil
}
