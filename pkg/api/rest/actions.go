package rest

import (
	"net/http"

	t "github.com/grioghar/lighthouse/pkg/types"
)

// scan triggers an update of all watched containers.
func (h *Handlers) scan(w http.ResponseWriter, _ *http.Request) {
	h.triggerAsync(w, nil)
}

// containerUpdate triggers an update scoped to a single container's image.
func (h *Handlers) containerUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c, err := h.deps.Client.GetContainer(t.ContainerID(id))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "container not found: " + err.Error()})
		return
	}
	h.triggerAsync(w, []string{c.ImageName()})
}

// triggerAsync acquires the shared update lock without blocking and runs the
// trigger in the background, mirroring the scheduler/legacy-endpoint semantics:
// only one update runs at a time. Returns 202 if started, 409 if one is already
// running, 503 if scanning isn't wired.
func (h *Handlers) triggerAsync(w http.ResponseWriter, images []string) {
	if h.deps.Lock == nil || h.deps.Trigger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "scanning is not enabled"})
		return
	}
	select {
	case v := <-h.deps.Lock:
		go func() {
			defer func() { h.deps.Lock <- v }()
			h.deps.Trigger(images)
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
	default:
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a scan is already running"})
	}
}
