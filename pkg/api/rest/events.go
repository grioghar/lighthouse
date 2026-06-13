package rest

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// Hub fans out log lines to connected Server-Sent-Events subscribers so the web
// UI can show live scan progress. It implements logrus.Hook, so registering it
// on the logger streams every log entry to the browser.
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[chan string]struct{})}
}

// Subscribe returns a new buffered channel that receives broadcast messages.
func (h *Hub) Subscribe() chan string {
	ch := make(chan string, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes and closes a previously subscribed channel.
func (h *Hub) Unsubscribe(ch chan string) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// Broadcast delivers msg to all subscribers, dropping it for any subscriber
// whose buffer is full (a slow client never blocks the logger).
func (h *Hub) Broadcast(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Levels implements logrus.Hook. Debug and Trace are deliberately excluded: the
// trace level is documented to include credentials/tokens, and the SSE stream
// surfaces log lines in the browser, so only Info and above are broadcast.
func (h *Hub) Levels() []logrus.Level {
	return []logrus.Level{
		logrus.PanicLevel,
		logrus.FatalLevel,
		logrus.ErrorLevel,
		logrus.WarnLevel,
		logrus.InfoLevel,
	}
}

// Fire implements logrus.Hook, broadcasting a one-line rendering of the entry.
func (h *Hub) Fire(e *logrus.Entry) error {
	msg := fmt.Sprintf("%s [%s] %s", e.Time.Format("15:04:05"), e.Level.String(), e.Message)
	if name, ok := e.Data["container"]; ok {
		msg += fmt.Sprintf(" (%v)", name)
	}
	h.Broadcast(msg)
	return nil
}

// events streams log lines to the client as Server-Sent Events.
func (h *Handlers) events(w http.ResponseWriter, r *http.Request) {
	if h.deps.Hub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "events are not enabled"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.deps.Hub.Subscribe()
	defer h.deps.Hub.Unsubscribe(ch)

	// SSE comment line — keeps the connection warm without firing onmessage.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// SSE data must not contain raw newlines.
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(msg, "\n", " "))
			flusher.Flush()
		}
	}
}
