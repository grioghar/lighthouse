// Package store keeps a bounded, in-memory history of recent update sessions so
// the web API can show what Lighthouse has done without a database. Each scan's
// types.Report is snapshotted into a JSON-ready Session at record time.
package store

import (
	"sync"
	"time"

	t "github.com/grioghar/lighthouse/pkg/types"
)

// Trigger describes what initiated a session.
const (
	TriggerSchedule = "schedule"
	TriggerAPI      = "api"
	TriggerStartup  = "startup"
)

// ContainerResult is the per-container outcome of a session.
type ContainerResult struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ImageName string `json:"imageName"`
	OldImage  string `json:"currentImageId,omitempty"`
	NewImage  string `json:"latestImageId,omitempty"`
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
}

// Session is a snapshot of a single update run.
type Session struct {
	ID         int               `json:"id"`
	Time       time.Time         `json:"time"`
	DurationMS int64             `json:"durationMs"`
	Trigger    string            `json:"trigger"`
	Scanned    int               `json:"scanned"`
	Updated    int               `json:"updated"`
	Failed     int               `json:"failed"`
	Skipped    int               `json:"skipped"`
	Containers []ContainerResult `json:"containers,omitempty"`
}

// Store is a thread-safe ring buffer of recent sessions.
type Store struct {
	mu       sync.RWMutex
	sessions []Session
	capacity int
	nextID   int
}

// New returns a Store retaining up to capacity most-recent sessions.
func New(capacity int) *Store {
	if capacity <= 0 {
		capacity = 50
	}
	return &Store{capacity: capacity, nextID: 1}
}

// Record snapshots a report into a new Session and stores it, returning the
// stored value. report may be nil (e.g. a skipped scan), recorded as an empty run.
func (s *Store) Record(report t.Report, start time.Time, dur time.Duration, trigger string) Session {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := Session{
		ID:         s.nextID,
		Time:       start,
		DurationMS: dur.Milliseconds(),
		Trigger:    trigger,
	}
	if report != nil {
		sess.Scanned = len(report.Scanned())
		sess.Updated = len(report.Updated())
		sess.Failed = len(report.Failed())
		sess.Skipped = len(report.Skipped())
		for _, cr := range report.All() {
			sess.Containers = append(sess.Containers, ContainerResult{
				ID:        cr.ID().ShortID(),
				Name:      cr.Name(),
				ImageName: cr.ImageName(),
				OldImage:  cr.CurrentImageID().ShortID(),
				NewImage:  cr.LatestImageID().ShortID(),
				State:     cr.State(),
				Error:     cr.Error(),
			})
		}
	}

	s.nextID++
	s.sessions = append(s.sessions, sess)
	if len(s.sessions) > s.capacity {
		s.sessions = s.sessions[len(s.sessions)-s.capacity:]
	}
	return sess
}

// List returns session summaries (without the per-container detail), most recent first.
func (s *Store) List() []Session {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Session, 0, len(s.sessions))
	for i := len(s.sessions) - 1; i >= 0; i-- {
		sess := s.sessions[i]
		sess.Containers = nil
		out = append(out, sess)
	}
	return out
}

// Get returns the full session with the given ID.
func (s *Store) Get(id int) (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, sess := range s.sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	return Session{}, false
}

// Last returns the most recent session, if any.
func (s *Store) Last() (Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.sessions) == 0 {
		return Session{}, false
	}
	last := s.sessions[len(s.sessions)-1]
	last.Containers = nil
	return last, true
}
