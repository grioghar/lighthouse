package update

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/grioghar/lighthouse/pkg/pkgmgr"
)

// Engine runs providers over their targets and collects the results.
type Engine struct {
	// Providers to run, in order.
	Providers []Provider
	// Apply enables the destructive half. False -- the default -- means Check
	// runs and Apply never does, which is what makes a scheduled run safe
	// against a hypervisor without every provider having to implement dry-run
	// for itself.
	Apply bool
	// Concurrency bounds simultaneous target checks within one provider. Zero
	// means DefaultConcurrency.
	//
	// This matters more than it looks: a node with 46 guests checked serially
	// over ssh spends the whole run waiting on connection setup. It is bounded
	// rather than unlimited because each check is an ssh connection and a
	// package-index refresh, and forty at once will have the node swapping.
	Concurrency int
	// Include, when non-empty, limits the run to targets whose ID or name
	// matches one of these glob patterns.
	Include []string
	// Exclude drops matching targets. Applied after Include.
	Exclude []string
	// OnResult is called as each result lands, for streaming progress. It may
	// be called from multiple goroutines.
	OnResult func(Result)
	// Now is overridable for tests.
	Now func() time.Time
}

// DefaultConcurrency is a compromise between a run that takes an hour and one
// that makes the node unusable while it runs.
const DefaultConcurrency = 4

// BatchApplier lets a provider apply every outdated target in one call
// instead of one at a time.
//
// The Docker path needs this and it is not a nicety. Updating containers
// individually loses the ordering that makes the update correct: linked
// containers have to stop and start in dependency order, a rolling restart is
// a property of the set rather than of any member, and a container whose
// dependency is being replaced must be restarted even though its own image
// did not change. A provider that owns that ordering gets handed the whole
// outdated set and reports back per target.
//
// Providers that do not implement it are applied one target at a time, which
// is right for package upgrades -- those are genuinely independent.
type BatchApplier interface {
	ApplyAll(ctx context.Context, outdated []Target) []Result
}

// Serializer lets a provider declare that its targets must be checked one at a
// time. Package managers take a global lock per machine, so two concurrent
// apt runs against the same host do not go faster -- one simply waits on the
// dpkg lock, or fails.
type Serializer interface {
	Serial() bool
}

// Report is the outcome of a whole run.
type Report struct {
	Started  time.Time
	Finished time.Time
	Results  []Result
	// Errors are failures to even enumerate targets, which are distinct from
	// a target that failed its check: a provider that cannot reach the node at
	// all produces one of these and no results.
	Errors []error
}

// Duration is how long the run took.
func (r Report) Duration() time.Duration { return r.Finished.Sub(r.Started) }

// Counts tallies the run by outcome.
func (r Report) Counts() map[Outcome]int {
	c := map[Outcome]int{}
	for _, res := range r.Results {
		c[res.Outcome]++
	}
	return c
}

// Actionable returns the results an operator needs to look at.
func (r Report) Actionable() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Outcome.Actionable() {
			out = append(out, res)
		}
	}
	return out
}

// RebootPending returns targets reporting that they need a restart.
func (r Report) RebootPending() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Reboot == pkgmgr.Yes {
			out = append(out, res)
		}
	}
	return out
}

// Summary is a one-line rendering for a log or a notification subject.
func (r Report) Summary() string {
	c := r.Counts()
	parts := []string{fmt.Sprintf("%d checked", len(r.Results))}
	for _, o := range []Outcome{Updated, Outdated, Failed, Skipped} {
		if c[o] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c[o], o))
		}
	}
	if n := len(r.RebootPending()); n > 0 {
		parts = append(parts, fmt.Sprintf("%d need reboot", n))
	}
	return strings.Join(parts, ", ")
}

// Run executes every provider and returns the collected report.
//
// A provider that fails to enumerate its targets does not stop the run. On a
// mixed deployment the Proxmox node being unreachable should not suppress the
// Docker results from the machine Lighthouse is actually on.
func (e *Engine) Run(ctx context.Context) Report {
	now := e.Now
	if now == nil {
		now = time.Now
	}
	rep := Report{Started: now()}

	for _, p := range e.Providers {
		select {
		case <-ctx.Done():
			rep.Errors = append(rep.Errors, ctx.Err())
			rep.Finished = now()
			return rep
		default:
		}

		targets, err := p.Discover(ctx)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("%s: discovering targets: %w", p.Name(), err))
			continue
		}
		targets = e.filter(targets)
		if len(targets) == 0 {
			continue
		}
		results := e.runProvider(ctx, p, targets)
		if batch, ok := p.(BatchApplier); ok && e.Apply {
			results = e.applyBatch(ctx, p, batch, results)
		}
		rep.Results = append(rep.Results, results...)
	}

	rep.Finished = now()
	sortResults(rep.Results)
	return rep
}

func (e *Engine) runProvider(ctx context.Context, p Provider, targets []Target) []Result {
	limit := e.Concurrency
	if limit <= 0 {
		limit = DefaultConcurrency
	}
	if s, ok := p.(Serializer); ok && s.Serial() {
		limit = 1
	}
	if limit > len(targets) {
		limit = len(targets)
	}

	results := make([]Result, len(targets))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup

	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = e.one(ctx, p, t)
			if e.OnResult != nil {
				e.OnResult(results[i])
			}
		}(i, t)
	}
	wg.Wait()
	return results
}

// applyBatch hands every outdated target to a provider that owns its own
// ordering, then folds the outcomes back into the per-target results.
func (e *Engine) applyBatch(ctx context.Context, p Provider, batch BatchApplier, checked []Result) []Result {
	var outdated []Target
	for _, r := range checked {
		if r.Outcome == Outdated {
			outdated = append(outdated, r.Target)
		}
	}
	if len(outdated) == 0 {
		return checked
	}

	applied := make(map[string]Result, len(outdated))
	for _, r := range batch.ApplyAll(ctx, outdated) {
		r.Provider = p.Name()
		applied[r.Target.ID] = r
	}

	for i, r := range checked {
		a, ok := applied[r.Target.ID]
		if !ok {
			if r.Outcome != Outdated {
				continue
			}
			// The provider was asked to update this and said nothing about it.
			// Silently reporting the check result would claim an update that
			// may not have happened.
			checked[i].Detail = r.Detail + " (provider returned no result for this target)"
			checked[i].Outcome = Failed
			continue
		}
		// Keep what the check learned; the apply result is authoritative only
		// about what it did.
		a.Duration = r.Duration
		if a.Installed == "" {
			a.Installed = r.Installed
		}
		if a.Available == "" {
			a.Available = r.Available
		}
		checked[i] = a
	}
	return checked
}

// one checks a single target and applies if warranted.
func (e *Engine) one(ctx context.Context, p Provider, t Target) Result {
	now := e.Now
	if now == nil {
		now = time.Now
	}
	start := now()

	if err := ctx.Err(); err != nil {
		return Result{Provider: p.Name(), Target: t, Outcome: Skipped,
			Detail: "cancelled before check", Err: err}
	}

	res := p.Check(ctx, t)
	res.Provider, res.Target = p.Name(), t

	// Apply only on Outdated. A provider that already did the work in Check
	// would be violating the contract, and one that failed its check has no
	// business being changed. Batch providers are skipped here and handled
	// once, afterwards, by applyBatch.
	_, isBatch := p.(BatchApplier)
	if e.Apply && res.Outcome == Outdated && !isBatch {
		applied := p.Apply(ctx, t)
		applied.Provider, applied.Target = p.Name(), t
		// Carry forward what the check learned, so a successful apply still
		// reports how many packages it moved.
		if applied.Pending == 0 {
			applied.Pending = res.Pending
		}
		if len(applied.Packages) == 0 {
			applied.Packages = res.Packages
		}
		if applied.Reboot == pkgmgr.Unknown {
			applied.Reboot = res.Reboot
		}
		res = applied
	} else if res.Outcome == Outdated && !e.Apply {
		if res.Detail == "" {
			res.Detail = "update available"
		}
		res.Detail += " (not applied: check-only run)"
	}

	res.Duration = now().Sub(start)
	if res.Err != nil && res.Outcome == "" {
		res.Outcome = Failed
	}
	if res.Outcome == "" {
		res.Outcome = UpToDate
	}
	return res
}

// filter applies the include and exclude patterns.
func (e *Engine) filter(targets []Target) []Target {
	if len(e.Include) == 0 && len(e.Exclude) == 0 {
		return targets
	}
	var out []Target
	for _, t := range targets {
		if len(e.Include) > 0 && !matchAny(e.Include, t) {
			continue
		}
		if matchAny(e.Exclude, t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// matchAny tests a target against glob patterns, over both its ID and its
// name. Matching both means `--exclude plex` and `--exclude ct:3111` are
// equally usable without the operator having to know which form we store.
func matchAny(patterns []string, t Target) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for _, subject := range []string{t.ID, t.Name} {
			if subject == "" {
				continue
			}
			if strings.EqualFold(subject, p) {
				return true
			}
			if ok, err := path.Match(strings.ToLower(p), strings.ToLower(subject)); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// sortResults orders the report: things needing attention first, then by
// provider and target, so a run's output is stable and the interesting lines
// are not buried under forty "up to date" rows.
func sortResults(rs []Result) {
	rank := map[Outcome]int{Failed: 0, Outdated: 1, Updated: 2, UpToDate: 3, Skipped: 4}
	sort.SliceStable(rs, func(i, j int) bool {
		if a, b := rank[rs[i].Outcome], rank[rs[j].Outcome]; a != b {
			return a < b
		}
		if rs[i].Provider != rs[j].Provider {
			return rs[i].Provider < rs[j].Provider
		}
		return rs[i].Target.ID < rs[j].Target.ID
	})
}
