package config

import (
	"path/filepath"
	"testing"
)

func TestNewStoreEmptyPathUsesDefaults(t *testing.T) {
	defaults := Settings{
		Cleanup:              true,
		MonitorOnly:          true,
		HealthGated:          true,
		HealthTimeoutSeconds: 30,
	}

	s, err := NewStore("", defaults)
	if err != nil {
		t.Fatalf("NewStore: unexpected error: %v", err)
	}

	if got := s.Get(); got != defaults {
		t.Errorf("Get() = %+v, want %+v", got, defaults)
	}
}

func TestSetGetRoundTrip(t *testing.T) {
	s, err := NewStore("", Settings{})
	if err != nil {
		t.Fatalf("NewStore: unexpected error: %v", err)
	}

	want := Settings{
		Cleanup:              true,
		NoRestart:            true,
		NoPull:               true,
		LifecycleHooks:       true,
		RollingRestart:       true,
		HealthGated:          true,
		HealthTimeoutSeconds: 120,
	}

	if err := s.Set(want); err != nil {
		t.Fatalf("Set: unexpected error: %v", err)
	}

	if got := s.Get(); got != want {
		t.Errorf("Get() = %+v, want %+v", got, want)
	}
}

func TestSetPersistsAndReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "settings.json")

	s, err := NewStore(path, Settings{})
	if err != nil {
		t.Fatalf("NewStore: unexpected error: %v", err)
	}

	want := Settings{
		Cleanup:              true,
		MonitorOnly:          true,
		LifecycleHooks:       true,
		HealthGated:          true,
		HealthTimeoutSeconds: 45,
	}

	if err := s.Set(want); err != nil {
		t.Fatalf("Set: unexpected error: %v", err)
	}

	// A second store with *different* defaults must load the persisted values,
	// proving on-disk state wins over defaults.
	otherDefaults := Settings{
		NoPull:               true,
		RollingRestart:       true,
		HealthTimeoutSeconds: 999,
	}

	s2, err := NewStore(path, otherDefaults)
	if err != nil {
		t.Fatalf("NewStore (reload): unexpected error: %v", err)
	}

	if got := s2.Get(); got != want {
		t.Errorf("reloaded Get() = %+v, want %+v", got, want)
	}
}

func TestNewStoreMissingFileUsesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")

	defaults := Settings{
		Cleanup:              true,
		HealthTimeoutSeconds: 60,
	}

	s, err := NewStore(path, defaults)
	if err != nil {
		t.Fatalf("NewStore: unexpected error for missing file: %v", err)
	}

	if got := s.Get(); got != defaults {
		t.Errorf("Get() = %+v, want %+v", got, defaults)
	}
}

func TestSetValidation(t *testing.T) {
	base := Settings{HealthTimeoutSeconds: 10}

	tests := []struct {
		name string
		v    Settings
	}{
		{
			name: "negative health timeout",
			v:    Settings{HealthTimeoutSeconds: -1},
		},
		{
			name: "rolling restart with monitor only",
			v:    Settings{RollingRestart: true, MonitorOnly: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := NewStore("", base)
			if err != nil {
				t.Fatalf("NewStore: unexpected error: %v", err)
			}

			if err := s.Set(tt.v); err == nil {
				t.Fatalf("Set(%+v) = nil error, want error", tt.v)
			}

			// A rejected Set must leave the stored value unchanged.
			if got := s.Get(); got != base {
				t.Errorf("after rejected Set, Get() = %+v, want unchanged %+v", got, base)
			}
		})
	}
}

func TestSetValidationDoesNotPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	base := Settings{HealthTimeoutSeconds: 5}
	s, err := NewStore(path, base)
	if err != nil {
		t.Fatalf("NewStore: unexpected error: %v", err)
	}

	if err := s.Set(Settings{HealthTimeoutSeconds: -1}); err == nil {
		t.Fatal("Set with negative timeout = nil error, want error")
	}

	// The invalid Set must not have created/written the file; a fresh store
	// with the same defaults should still see defaults.
	s2, err := NewStore(path, base)
	if err != nil {
		t.Fatalf("NewStore (reload): unexpected error: %v", err)
	}
	if got := s2.Get(); got != base {
		t.Errorf("reloaded Get() = %+v, want defaults %+v", got, base)
	}
}
