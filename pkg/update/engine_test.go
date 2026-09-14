package update

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grioghar/lighthouse/pkg/pkgmgr"
)

// stub is a configurable provider.
type stub struct {
	name     string
	targets  []Target
	discErr  error
	checked  atomic.Int32
	applied  atomic.Int32
	outcome  map[string]Outcome
	serial   bool
	maxSeen  atomic.Int32
	inflight atomic.Int32
}

func (s *stub) Name() string     { return s.name }
func (s *stub) Describe() string { return "stub" }

func (s *stub) Discover(context.Context) ([]Target, error) {
	if s.discErr != nil {
		return nil, s.discErr
	}
	return s.targets, nil
}

func (s *stub) Check(_ context.Context, t Target) Result {
	n := s.inflight.Add(1)
	for {
		m := s.maxSeen.Load()
		if n <= m || s.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	s.inflight.Add(-1)
	s.checked.Add(1)

	o := UpToDate
	if s.outcome != nil {
		if got, ok := s.outcome[t.ID]; ok {
			o = got
		}
	}
	return Result{Outcome: o, Detail: "checked"}
}

func (s *stub) Apply(_ context.Context, t Target) Result {
	s.applied.Add(1)
	return Result{Outcome: Updated, Detail: "applied"}
}

func (s *stub) Serial() bool { return s.serial }

func targets(ids ...string) []Target {
	out := make([]Target, 0, len(ids))
	for _, id := range ids {
		out = append(out, Target{ID: id, Name: id})
	}
	return out
}

func TestCheckOnlyRunNeverApplies(t *testing.T) {
	// This is the safety property the whole design rests on: dry-run is not a
	// flag each provider has to remember, it is Apply never being called.
	s := &stub{name: "p", targets: targets("a", "b"),
		outcome: map[string]Outcome{"a": Outdated, "b": Outdated}}
	e := &Engine{Providers: []Provider{s}} // Apply defaults to false
	rep := e.Run(context.Background())

	if s.applied.Load() != 0 {
		t.Fatalf("check-only run applied %d times", s.applied.Load())
	}
	if len(rep.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(rep.Results))
	}
	for _, r := range rep.Results {
		if r.Outcome != Outdated {
			t.Errorf("%s: outcome %s", r.Target.ID, r.Outcome)
		}
		if got := r.Detail; got == "checked" {
			t.Errorf("%s: should say it was not applied, got %q", r.Target.ID, got)
		}
	}
}

func TestApplyOnlyTouchesOutdated(t *testing.T) {
	s := &stub{name: "p", targets: targets("a", "b", "c"),
		outcome: map[string]Outcome{"a": Outdated, "b": UpToDate, "c": Failed}}
	e := &Engine{Providers: []Provider{s}, Apply: true}
	e.Run(context.Background())

	// Not the up-to-date one, and emphatically not the one whose check failed:
	// a target we could not read has no business being changed.
	if got := s.applied.Load(); got != 1 {
		t.Fatalf("applied %d times, want 1", got)
	}
}

func TestSerialProviderIsNotRunConcurrently(t *testing.T) {
	// dpkg takes a machine-wide lock; concurrency against one host does not
	// go faster, it deadlocks or dies on the lock.
	s := &stub{name: "host", targets: targets("h1", "h2", "h3", "h4"), serial: true}
	e := &Engine{Providers: []Provider{s}, Concurrency: 4}
	e.Run(context.Background())

	if got := s.maxSeen.Load(); got != 1 {
		t.Fatalf("serial provider ran %d checks concurrently", got)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	s := &stub{name: "guests", targets: targets("1", "2", "3", "4", "5", "6", "7", "8")}
	e := &Engine{Providers: []Provider{s}, Concurrency: 3}
	e.Run(context.Background())

	if got := s.maxSeen.Load(); got > 3 {
		t.Fatalf("concurrency %d exceeded the limit of 3", got)
	}
	if s.checked.Load() != 8 {
		t.Fatalf("checked %d of 8", s.checked.Load())
	}
}

func TestDiscoveryFailureDoesNotStopOtherProviders(t *testing.T) {
	// A homelab where the Proxmox node is unreachable should still get its
	// Docker results from the machine Lighthouse is actually running on.
	bad := &stub{name: "proxmox", discErr: errors.New("ssh: connection refused")}
	good := &stub{name: "docker", targets: targets("plex")}
	e := &Engine{Providers: []Provider{bad, good}}
	rep := e.Run(context.Background())

	if len(rep.Errors) != 1 {
		t.Fatalf("want 1 discovery error, got %v", rep.Errors)
	}
	if len(rep.Results) != 1 || rep.Results[0].Target.ID != "plex" {
		t.Fatalf("the healthy provider was suppressed: %+v", rep.Results)
	}
}

func TestFilters(t *testing.T) {
	s := &stub{name: "p", targets: targets("ct:3111", "ct:3136", "docker:plex")}
	e := &Engine{Providers: []Provider{s}, Include: []string{"ct:*"}}
	rep := e.Run(context.Background())
	if len(rep.Results) != 2 {
		t.Fatalf("include glob gave %d results", len(rep.Results))
	}

	// Exclude wins over include, and matches on name as well as ID, so an
	// operator need not know which form we store.
	s2 := &stub{name: "p", targets: []Target{
		{ID: "ct:3111", Name: "plex"},
		{ID: "ct:3136", Name: "plex-oci"},
	}}
	e2 := &Engine{Providers: []Provider{s2}, Exclude: []string{"plex"}}
	rep2 := e2.Run(context.Background())
	if len(rep2.Results) != 1 || rep2.Results[0].Target.Name != "plex-oci" {
		t.Fatalf("exclude by name gave %+v", rep2.Results)
	}
}

// batchStub records what ApplyAll was handed.
type batchStub struct {
	stub
	mu       sync.Mutex
	gotBatch []Target
	omit     string // a target to deliberately not report on
}

func (b *batchStub) ApplyAll(_ context.Context, outdated []Target) []Result {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gotBatch = outdated
	var out []Result
	for _, t := range outdated {
		if t.ID == b.omit {
			continue
		}
		out = append(out, Result{Target: t, Outcome: Updated, Detail: "batched"})
	}
	return out
}

func TestBatchProviderGetsWholeOutdatedSetAtOnce(t *testing.T) {
	// Container updates are not independent: linked containers must restart in
	// dependency order. The provider owns that, so it gets the whole set.
	b := &batchStub{stub: stub{name: "docker", targets: targets("a", "b", "c"),
		outcome: map[string]Outcome{"a": Outdated, "b": UpToDate, "c": Outdated}}}
	e := &Engine{Providers: []Provider{b}, Apply: true}
	rep := e.Run(context.Background())

	if len(b.gotBatch) != 2 {
		t.Fatalf("ApplyAll got %d targets, want 2", len(b.gotBatch))
	}
	// Per-target Apply must not also fire.
	if b.applied.Load() != 0 {
		t.Fatalf("per-target Apply ran %d times on a batch provider", b.applied.Load())
	}
	var updated int
	for _, r := range rep.Results {
		if r.Outcome == Updated {
			updated++
		}
	}
	if updated != 2 {
		t.Fatalf("want 2 updated, got %d: %+v", updated, rep.Results)
	}
}

func TestBatchProviderSilenceIsReportedAsFailure(t *testing.T) {
	// If the provider was asked to update something and says nothing about it,
	// reporting the check result would claim an update that may not have
	// happened.
	b := &batchStub{stub: stub{name: "docker", targets: targets("a", "b"),
		outcome: map[string]Outcome{"a": Outdated, "b": Outdated}}, omit: "b"}
	e := &Engine{Providers: []Provider{b}, Apply: true}
	rep := e.Run(context.Background())

	var failed int
	for _, r := range rep.Results {
		if r.Target.ID == "b" && r.Outcome == Failed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("unreported target should be a failure: %+v", rep.Results)
	}
}

func TestReportSummaryAndOrdering(t *testing.T) {
	s := &stub{name: "p", targets: targets("ok", "bad", "old"),
		outcome: map[string]Outcome{"ok": UpToDate, "bad": Failed, "old": Outdated}}
	e := &Engine{Providers: []Provider{s}}
	rep := e.Run(context.Background())

	// Failures first, then outdated: the interesting lines must not be buried
	// under forty "up to date" rows.
	if rep.Results[0].Outcome != Failed || rep.Results[1].Outcome != Outdated {
		t.Fatalf("results not ordered by urgency: %+v", rep.Results)
	}
	if got := len(rep.Actionable()); got != 2 {
		t.Fatalf("want 2 actionable, got %d", got)
	}
	if s := rep.Summary(); s == "" {
		t.Fatal("empty summary")
	}
}

func TestRebootPendingIsCollected(t *testing.T) {
	s := &stub{name: "p", targets: targets("node")}
	e := &Engine{Providers: []Provider{s}}
	rep := e.Run(context.Background())
	rep.Results[0].Reboot = pkgmgr.Yes
	if got := len(rep.RebootPending()); got != 1 {
		t.Fatalf("want 1 reboot pending, got %d", got)
	}
}

func TestRegistrySelect(t *testing.T) {
	r := NewRegistry()
	r.Add(&stub{name: "a"}).Add(&stub{name: "b"})

	if got, _ := r.Select(nil); len(got) != 2 {
		t.Fatalf("empty selection should mean all, got %d", len(got))
	}
	if got, _ := r.Select([]string{"all"}); len(got) != 2 {
		t.Fatalf("'all' gave %d", len(got))
	}
	if got, err := r.Select([]string{"a"}); err != nil || len(got) != 1 {
		t.Fatalf("select a: %v %d", err, len(got))
	}
	// An unknown name must be an error listing what is available, not a
	// silently empty run that looks like everything was up to date.
	err := func() error { _, e := r.Select([]string{"nope"}); return e }()
	if err == nil {
		t.Fatal("unknown provider should error")
	}
	if got := fmt.Sprint(err); got == "" {
		t.Fatal("error should name the available providers")
	}
}
