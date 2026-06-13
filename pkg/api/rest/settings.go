package rest

import (
	"encoding/json"
	"net/http"

	"github.com/grioghar/lighthouse/pkg/config"
)

// SettingsValue returns the current runtime settings, or the zero value if no
// settings store is configured. Used by the web UI to render the settings form.
func (h *Handlers) SettingsValue() config.Settings {
	if h.deps.Settings == nil {
		return config.Settings{}
	}
	return h.deps.Settings.Get()
}

// SettingsEnabled reports whether a runtime settings store is configured.
func (h *Handlers) SettingsEnabled() bool {
	return h.deps.Settings != nil
}

func (h *Handlers) settingsGet(w http.ResponseWriter, _ *http.Request) {
	if h.deps.Settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "runtime settings are not enabled"})
		return
	}
	writeJSON(w, http.StatusOK, h.deps.Settings.Get())
}

func (h *Handlers) settingsPost(w http.ResponseWriter, r *http.Request) {
	if h.deps.Settings == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "runtime settings are not enabled"})
		return
	}
	var s config.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid settings payload: " + err.Error()})
		return
	}
	if err := h.deps.Settings.Set(s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, h.deps.Settings.Get())
}
