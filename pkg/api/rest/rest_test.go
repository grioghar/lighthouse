package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/grioghar/lighthouse/internal/actions/mocks"
	"github.com/grioghar/lighthouse/pkg/api/store"
	t "github.com/grioghar/lighthouse/pkg/types"
)

func testHandlers() (*Handlers, *store.Store, chan bool) {
	data := &mocks.TestData{
		Containers: []t.Container{
			mocks.CreateMockContainerWithConfig("c1", "c1", "img:latest", true, false, time.Now(), &dockerContainer.Config{}),
			mocks.CreateMockContainerWithConfig("c2", "c2", "img2:latest", true, false, time.Now(), &dockerContainer.Config{}),
		},
	}
	st := store.New(10)
	lock := make(chan bool, 1)
	lock <- true
	deps := Deps{
		Version: "v-test",
		Client:  mocks.CreateMockClient(data, false, false),
		Store:   st,
		Trigger: func(_ []string) {},
		Lock:    lock,
		Config:  ConfigInfo{Schedule: "@daily"},
	}
	return New(deps), st, lock
}

func TestStatusEndpoint(t *testing.T) {
	h, _, _ := testHandlers()
	rec := httptest.NewRecorder()
	h.status(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var dto StatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Version != "v-test" || !dto.DaemonReachable || dto.WatchedContainers != 2 {
		t.Fatalf("unexpected status dto: %+v", dto)
	}
}

func TestContainersEndpoint(t *testing.T) {
	h, _, _ := testHandlers()
	rec := httptest.NewRecorder()
	h.containers(rec, httptest.NewRequest(http.MethodGet, "/api/v1/containers", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var out []ContainerDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d containers, want 2", len(out))
	}
}

func TestSessionsEndpoint(t *testing.T) {
	h, st, _ := testHandlers()
	st.Record(nil, time.Now(), time.Second, store.TriggerAPI)

	rec := httptest.NewRecorder()
	h.sessions(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", rec.Code)
	}
	var out []store.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d sessions, want 1", len(out))
	}
}

func TestScanStartsAndConflicts(t *testing.T) {
	// Lock available -> 202 Accepted.
	h, _, _ := testHandlers()
	rec := httptest.NewRecorder()
	h.scan(rec, httptest.NewRequest(http.MethodPost, "/api/v1/scan", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scan with free lock = %d, want 202", rec.Code)
	}

	// Lock held -> 409 Conflict.
	h2, _, lock := testHandlers()
	<-lock // hold the lock so the scan can't acquire it
	rec = httptest.NewRecorder()
	h2.scan(rec, httptest.NewRequest(http.MethodPost, "/api/v1/scan", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("scan with held lock = %d, want 409", rec.Code)
	}
}
