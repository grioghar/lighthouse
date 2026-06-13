package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/grioghar/lighthouse/internal/actions/mocks"
	"github.com/grioghar/lighthouse/pkg/api/auth"
	"github.com/grioghar/lighthouse/pkg/api/rest"
	"github.com/grioghar/lighthouse/pkg/api/store"
	t "github.com/grioghar/lighthouse/pkg/types"
)

func newTestServer(tb testing.TB) *Server {
	tb.Helper()
	data := &mocks.TestData{
		Containers: []t.Container{
			mocks.CreateMockContainerWithConfig("c1", "c1", "img:latest", true, false, time.Now(), &dockerContainer.Config{}),
		},
	}
	deps := rest.Deps{
		Version: "v-test",
		Client:  mocks.CreateMockClient(data, false, false),
		Store:   store.New(5),
		Config:  rest.ConfigInfo{Schedule: "@daily"},
	}
	s, err := New(auth.New("tok", "key"), rest.New(deps), false)
	if err != nil {
		tb.Fatalf("New: %v", err) // also fails if templates don't parse
	}
	return s
}

func TestLoginPageRenders(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.loginPage(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Sign in") {
		t.Fatalf("login page missing expected content: %q", body)
	}
}

func TestDashboardRenders(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.dashboard(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Lighthouse", "Watched containers", "c1", "Scan now"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
}

func TestContainersFragmentRenders(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.containersFragment(rec, httptest.NewRequest(http.MethodGet, "/ui/containers", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "c1") || !strings.Contains(body, "running") {
		t.Fatalf("containers fragment render failed: code=%d body=%q", rec.Code, body)
	}
}
