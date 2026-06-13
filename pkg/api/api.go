package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"time"

	"github.com/grioghar/lighthouse/pkg/api/auth"
	"github.com/grioghar/lighthouse/pkg/api/rest"
	"github.com/grioghar/lighthouse/pkg/api/web"
	log "github.com/sirupsen/logrus"
)

const tokenMissingMsg = "api token is empty or has not been set. exiting"

// ListenAddr is the default address the HTTP API/UI binds to.
const ListenAddr = ":8080"

// API is the http server responsible for serving the HTTP API endpoints and,
// when enabled, the web administration UI.
type API struct {
	Token       string
	Address     string
	TLSCert     string
	TLSKey      string
	hasHandlers bool
	streaming   bool
	mux         *http.ServeMux
}

// TLSEnabled reports whether both a TLS certificate and key are configured.
func (api *API) TLSEnabled() bool {
	return api.TLSCert != "" && api.TLSKey != ""
}

// New is a factory function creating a new API instance
func New(token string) *API {
	return &API{
		Token:       token,
		Address:     ListenAddr,
		hasHandlers: false,
		mux:         http.NewServeMux(),
	}
}

// RequireToken is wrapper around http.HandleFunc that checks token validity
func (api *API) RequireToken(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		want := fmt.Sprintf("Bearer %s", api.Token)
		// Constant-time comparison to avoid leaking the token via timing.
		if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		log.Debug("Valid token found.")
		fn(w, r)
	}
}

// RegisterFunc is a wrapper around http.HandleFunc that also sets the flag used to determine whether to launch the API
func (api *API) RegisterFunc(path string, fn http.HandlerFunc) {
	api.hasHandlers = true
	api.mux.HandleFunc(path, api.RequireToken(fn))
}

// RegisterHandler is a wrapper around http.Handler that also sets the flag used to determine whether to launch the API
func (api *API) RegisterHandler(path string, handler http.Handler) {
	api.hasHandlers = true
	api.mux.HandleFunc(path, api.RequireToken(handler.ServeHTTP))
}

// EnableWeb mounts the JSON API (/api/v1, session- or bearer-authenticated) and
// the server-rendered web UI. secret optionally pins the session signing key;
// secure marks session cookies Secure (for TLS deployments).
func (api *API) EnableWeb(deps rest.Deps, secret string, secure bool) error {
	if api.Token == "" {
		log.Fatal(tokenMissingMsg)
	}
	authn := auth.New(api.Token, secret)
	handlers := rest.New(deps)
	handlers.Register(api.mux, authn.RequireAPI)

	ui, err := web.New(authn, handlers, secure)
	if err != nil {
		return err
	}
	ui.Register(api.mux)

	api.hasHandlers = true
	api.streaming = true // the /api/v1/events SSE stream is long-lived
	log.Infof("Web UI enabled on %s", api.Address)
	return nil
}

// Start the API and serve over HTTP. Requires an API Token to be set.
func (api *API) Start(block bool) error {

	if !api.hasHandlers {
		log.Debug("Lighthouse HTTP API skipped.")
		return nil
	}

	if api.Token == "" {
		log.Fatal(tokenMissingMsg)
	}

	// When the SSE event stream is enabled, responses are long-lived, so a
	// global WriteTimeout would cut them off. ReadHeaderTimeout still guards
	// against slow-header (Slowloris) attacks either way.
	writeTimeout := 30 * time.Second
	if api.streaming {
		writeTimeout = 0
	}

	server := &http.Server{
		Addr:              api.Address,
		Handler:           api.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}

	if block {
		runHTTPServer(server, api.TLSCert, api.TLSKey)
	} else {
		go func() {
			runHTTPServer(server, api.TLSCert, api.TLSKey)
		}()
	}
	return nil
}

func runHTTPServer(server *http.Server, certFile, keyFile string) {
	if certFile != "" && keyFile != "" {
		log.Fatal(server.ListenAndServeTLS(certFile, keyFile))
		return
	}
	log.Fatal(server.ListenAndServe())
}
