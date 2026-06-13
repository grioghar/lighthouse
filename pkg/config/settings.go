// Package config provides a thread-safe, JSON-file-backed store of
// runtime-adjustable update settings, so the web UI can change update behavior
// at runtime and have it persist across restarts.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Settings holds the runtime-adjustable update behavior. JSON-tagged for the API
// and for on-disk persistence.
type Settings struct {
	Cleanup              bool `json:"cleanup"`
	NoRestart            bool `json:"noRestart"`
	MonitorOnly          bool `json:"monitorOnly"`
	NoPull               bool `json:"noPull"`
	LifecycleHooks       bool `json:"lifecycleHooks"`
	RollingRestart       bool `json:"rollingRestart"`
	HealthGated          bool `json:"healthGated"`
	HealthTimeoutSeconds int  `json:"healthTimeoutSeconds"`
}

// validate checks the settings for internal consistency. The rules mirror
// constraints enforced elsewhere in the app.
func (v Settings) validate() error {
	if v.HealthTimeoutSeconds < 0 {
		return errors.New("health timeout seconds must be >= 0")
	}
	if v.RollingRestart && v.MonitorOnly {
		return errors.New("rolling restart is not compatible with monitor-only")
	}
	return nil
}

// Store is a concurrency-safe holder of Settings, optionally persisted to a file.
type Store struct {
	mu      sync.RWMutex
	current Settings
	path    string
}

// NewStore returns a Store initialized to defaults. If path is non-empty and the
// file exists and parses, the persisted values replace the defaults. If path is
// non-empty but the file does not exist, defaults are used (and a later Set will
// create the file). A malformed file returns an error.
func NewStore(path string, defaults Settings) (*Store, error) {
	s := &Store{
		current: defaults,
		path:    path,
	}

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			// Unmarshal into a copy of defaults so unspecified fields keep
			// their default values.
			loaded := defaults
			if err := json.Unmarshal(data, &loaded); err != nil {
				return nil, fmt.Errorf("config: parsing settings file %q: %w", path, err)
			}
			s.current = loaded
		case os.IsNotExist(err):
			// No file yet: keep defaults. A later Set will create it.
		default:
			return nil, fmt.Errorf("config: reading settings file %q: %w", path, err)
		}
	}

	return s, nil
}

// Get returns a copy of the current settings (safe for concurrent use).
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Set validates v, replaces the current settings, and (if a path was configured)
// persists them atomically (write to a temp file, then rename). Returns an error
// on validation failure or write failure.
func (s *Store) Set(v Settings) error {
	if err := v.validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path != "" {
		if err := persist(s.path, v); err != nil {
			return err
		}
	}

	s.current = v
	return nil
}

// persist writes v to path atomically: it writes to a temp file in the same
// directory, then renames it over path.
func persist(path string, v Settings) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding settings: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: creating settings dir %q: %w", dir, err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("config: writing temp settings file %q: %w", tmp, err)
	}

	if err := os.Rename(tmp, path); err != nil {
		// Best-effort cleanup of the temp file on rename failure.
		_ = os.Remove(tmp)
		return fmt.Errorf("config: renaming %q to %q: %w", tmp, path, err)
	}

	return nil
}
