package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"time"

	log "github.com/sirupsen/logrus"
)

const tokenMissingMsg = "api token is empty or has not been set. exiting"

// ListenAddr is the address the HTTP API binds to.
// TODO: make this configurable via a flag.
const ListenAddr = ":8080"

// API is the http server responsible for serving the HTTP API endpoints
type API struct {
	Token       string
	hasHandlers bool
	mux         *http.ServeMux
}

// New is a factory function creating a new API instance
func New(token string) *API {
	return &API{
		Token:       token,
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

// Start the API and serve over HTTP. Requires an API Token to be set.
func (api *API) Start(block bool) error {

	if !api.hasHandlers {
		log.Debug("Watchtower HTTP API skipped.")
		return nil
	}

	if api.Token == "" {
		log.Fatal(tokenMissingMsg)
	}

	server := &http.Server{
		Addr:              ListenAddr,
		Handler:           api.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	if block {
		runHTTPServer(server)
	} else {
		go func() {
			runHTTPServer(server)
		}()
	}
	return nil
}

func runHTTPServer(server *http.Server) {
	log.Fatal(server.ListenAndServe())
}
