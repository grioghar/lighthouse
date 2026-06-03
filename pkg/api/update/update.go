package update

import (
	"io"
	"net/http"
	"strings"

	log "github.com/sirupsen/logrus"
)

// maxBodyBytes caps how much of the request body we will read, to avoid a
// memory-exhaustion DoS from an authenticated but malicious caller.
const maxBodyBytes = 4 << 10 // 4 KiB

var (
	lock chan bool
)

// New is a factory function creating a new  Handler instance
func New(updateFn func(images []string), updateLock chan bool) *Handler {
	if updateLock != nil {
		lock = updateLock
	} else {
		lock = make(chan bool, 1)
		lock <- true
	}

	return &Handler{
		fn:   updateFn,
		Path: "/v1/update",
	}
}

// Handler is an API handler used for triggering container update scans
type Handler struct {
	fn   func(images []string)
	Path string
}

// Handle is the actual http.Handle function doing all the heavy lifting
func (handle *Handler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	log.Info("Updates triggered by HTTP API request.")

	// Drain (a bounded amount of) the body so the connection can be reused,
	// without copying attacker-controlled data to stdout or reading unbounded.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxBodyBytes))

	var images []string
	imageQueries, found := r.URL.Query()["image"]
	if found {
		for _, image := range imageQueries {
			images = append(images, strings.Split(image, ",")...)
		}

	} else {
		images = nil
	}

	if len(images) > 0 {
		chanValue := <-lock
		defer func() { lock <- chanValue }()
		handle.fn(images)
	} else {
		select {
		case chanValue := <-lock:
			defer func() { lock <- chanValue }()
			handle.fn(images)
		default:
			log.Debug("Skipped. Another update already running.")
		}
	}

}
