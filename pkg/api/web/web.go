// Package web serves the Lighthouse administration UI: server-rendered HTML
// (html/template) progressively enhanced with htmx, all assets embedded so the
// binary stays self-contained. It reads through the same rest.Handlers data
// layer that backs the JSON API.
package web

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/grioghar/lighthouse/pkg/api/auth"
	"github.com/grioghar/lighthouse/pkg/api/rest"
	"github.com/grioghar/lighthouse/pkg/api/store"
	"github.com/grioghar/lighthouse/pkg/config"
	log "github.com/sirupsen/logrus"
)

//go:embed templates/*.html
//go:embed static/*
var assets embed.FS

// Server renders and serves the web UI.
type Server struct {
	auth    *auth.Authenticator
	api     *rest.Handlers
	tmpl    *template.Template
	secure  bool
	limiter *auth.RateLimiter
}

// New parses the embedded templates and returns a UI Server.
func New(a *auth.Authenticator, api *rest.Handlers, secure bool) (*Server, error) {
	tmpl, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		auth:    a,
		api:     api,
		tmpl:    tmpl,
		secure:  secure,
		limiter: auth.NewRateLimiter(10, time.Minute),
	}, nil
}

// Register mounts the UI routes on mux.
func (s *Server) Register(mux *http.ServeMux) {
	staticFS, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)

	mux.Handle("GET /{$}", s.auth.RequireUI(http.HandlerFunc(s.dashboard)))
	mux.Handle("GET /ui/status", s.auth.RequireUI(http.HandlerFunc(s.statusFragment)))
	mux.Handle("GET /ui/containers", s.auth.RequireUI(http.HandlerFunc(s.containersFragment)))
	mux.Handle("GET /ui/history", s.auth.RequireUI(http.HandlerFunc(s.historyFragment)))
}

type dashboardVM struct {
	Status          rest.StatusDTO
	Containers      []rest.ContainerDTO
	ContainersErr   string
	Sessions        []store.Session
	Settings        config.Settings
	SettingsEnabled bool
}

func (s *Server) buildVM() dashboardVM {
	vm := dashboardVM{
		Status:          s.api.Status(),
		Sessions:        s.api.Store().List(),
		Settings:        s.api.SettingsValue(),
		SettingsEnabled: s.api.SettingsEnabled(),
	}
	cs, err := s.api.Containers()
	if err != nil {
		vm.ContainersErr = err.Error()
	} else {
		vm.Containers = cs
	}
	return vm
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.WithError(err).Error("web: template render failed")
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	// Ensure a CSRF token cookie exists so the UI can echo it on POSTs.
	if _, err := r.Cookie(auth.CSRFCookieName); err != nil {
		auth.SetCSRFCookie(w, auth.NewCSRFToken(), s.secure)
	}
	s.render(w, "layout", s.buildVM())
}

func (s *Server) statusFragment(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "statusCards", s.api.Status())
}

func (s *Server) containersFragment(w http.ResponseWriter, _ *http.Request) {
	vm := s.buildVM()
	s.render(w, "containersTable", vm)
}

func (s *Server) historyFragment(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "historyList", s.api.Store().List())
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.Authorized(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", map[string]any{})
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow(auth.ClientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", map[string]any{"Error": "Too many attempts. Please wait a moment and try again."})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if s.auth.CheckToken(r.PostFormValue("token")) {
		s.auth.SetSession(w, s.secure)
		auth.SetCSRFCookie(w, auth.NewCSRFToken(), s.secure)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, "login", map[string]any{"Error": "Invalid token."})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.auth.ClearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
