package rest

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/grioghar/lighthouse/pkg/api/store"
)

// ContainerDTO is the JSON shape for a watched container in list views.
type ContainerDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Image      string `json:"image"`
	ImageID    string `json:"imageId"`
	Running    bool   `json:"running"`
	Restarting bool   `json:"restarting"`
	Health     string `json:"health,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

// StatusDTO is the JSON shape for the dashboard header / status endpoint.
type StatusDTO struct {
	Version           string         `json:"version"`
	DaemonReachable   bool           `json:"daemonReachable"`
	WatchedContainers int            `json:"watchedContainers"`
	ScanRunning       bool           `json:"scanRunning"`
	Schedule          string         `json:"schedule,omitempty"`
	Filtering         string         `json:"filtering,omitempty"`
	LastScan          *store.Session `json:"lastScan,omitempty"`
}

// Store exposes the session history store (used by the web UI templates).
func (h *Handlers) Store() *store.Store { return h.deps.Store }

// Status builds the current status snapshot.
func (h *Handlers) Status() StatusDTO {
	daemonReachable := true
	watched := 0
	if cs, err := h.deps.Client.ListContainers(h.deps.Filter); err != nil {
		daemonReachable = false
	} else {
		watched = len(cs)
	}

	dto := StatusDTO{
		Version:           h.deps.Version,
		DaemonReachable:   daemonReachable,
		WatchedContainers: watched,
		ScanRunning:       h.scanRunning(),
		Schedule:          h.deps.Config.Schedule,
		Filtering:         h.deps.Config.Filtering,
	}
	if last, ok := h.deps.Store.Last(); ok {
		dto.LastScan = &last
	}
	return dto
}

// Containers builds the watched-container list (with best-effort health).
func (h *Handlers) Containers() ([]ContainerDTO, error) {
	cs, err := h.deps.Client.ListContainers(h.deps.Filter)
	if err != nil {
		return nil, err
	}

	out := make([]ContainerDTO, 0, len(cs))
	for _, c := range cs {
		dto := ContainerDTO{
			ID:         c.ID().ShortID(),
			Name:       strings.TrimPrefix(c.Name(), "/"),
			Image:      c.ImageName(),
			ImageID:    c.ImageID().ShortID(),
			Running:    c.IsRunning(),
			Restarting: c.IsRestarting(),
		}
		if scope, ok := c.Scope(); ok {
			dto.Scope = scope
		}
		if hs, err := h.deps.Client.GetContainerHealth(c.ID()); err == nil {
			dto.Health = hs.Status
		}
		out = append(out, dto)
	}
	return out, nil
}

func (h *Handlers) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.Status())
}

func (h *Handlers) containers(w http.ResponseWriter, _ *http.Request) {
	out, err := h.Containers()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not list containers: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) sessions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.deps.Store.List())
}

func (h *Handlers) sessionDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session id"})
		return
	}
	sess, ok := h.deps.Store.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (h *Handlers) config(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.deps.Config)
}
