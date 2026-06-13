// Package rest implements the Lighthouse JSON API that backs the web UI.
//
// Handlers are intentionally thin: they read from the container client and the
// in-memory session store, and trigger scans through the same lock + trigger
// function used by the scheduler and the legacy /v1/update endpoint.
package rest

import (
	"encoding/json"
	"net/http"

	"github.com/grioghar/lighthouse/pkg/api/store"
	"github.com/grioghar/lighthouse/pkg/container"
	t "github.com/grioghar/lighthouse/pkg/types"
)

// ConfigInfo is the redacted, read-only view of the running configuration. It
// deliberately contains no secrets (API token, notification URLs/passwords).
type ConfigInfo struct {
	Schedule       string   `json:"schedule,omitempty"`
	Filtering      string   `json:"filtering,omitempty"`
	Cleanup        bool     `json:"cleanup"`
	NoRestart      bool     `json:"noRestart"`
	NoPull         bool     `json:"noPull"`
	MonitorOnly    bool     `json:"monitorOnly"`
	LabelEnable    bool     `json:"labelEnable"`
	RollingRestart bool     `json:"rollingRestart"`
	LifecycleHooks bool     `json:"lifecycleHooks"`
	HealthGated    bool     `json:"healthGated"`
	HealthTimeout  string   `json:"healthTimeout,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	Notifiers      []string `json:"notifiers,omitempty"`
}

// Deps are the runtime dependencies the API handlers need.
type Deps struct {
	Version string
	Client  container.Client
	Store   *store.Store
	Filter  t.Filter
	// Trigger runs an update for the given image names (empty = all watched
	// containers). It is the same closure used by the scheduler.
	Trigger func(images []string)
	// Lock is the shared single-update lock; an empty channel means a scan is
	// currently running.
	Lock   chan bool
	Config ConfigInfo
	Hub    *Hub
}

// Handlers serves the JSON API.
type Handlers struct {
	deps Deps
}

// New returns API handlers bound to the given dependencies.
func New(deps Deps) *Handlers {
	return &Handlers{deps: deps}
}

// Register mounts the API routes on mux, wrapping each with wrap (the auth
// middleware). Routes use Go 1.22+ method+path patterns.
func (h *Handlers) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	route := func(pattern string, fn http.HandlerFunc) {
		mux.Handle(pattern, wrap(fn))
	}
	route("GET /api/v1/status", h.status)
	route("GET /api/v1/containers", h.containers)
	route("GET /api/v1/sessions", h.sessions)
	route("GET /api/v1/sessions/{id}", h.sessionDetail)
	route("GET /api/v1/config", h.config)
	route("POST /api/v1/scan", h.scan)
	route("POST /api/v1/containers/{id}/update", h.containerUpdate)
	route("GET /api/v1/events", h.events)
}

// scanRunning reports whether an update is currently in progress.
func (h *Handlers) scanRunning() bool {
	return h.deps.Lock != nil && len(h.deps.Lock) == 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
