package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewCSRFToken(t *testing.T) {
	a := NewCSRFToken()
	b := NewCSRFToken()
	if a == "" || b == "" {
		t.Fatal("expected non-empty tokens")
	}
	if a == b {
		t.Fatal("expected distinct tokens")
	}
}

func csrfRequest(cookie, header string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: cookie})
	}
	if header != "" {
		r.Header.Set(CSRFHeaderName, header)
	}
	return r
}

func TestValidCSRF(t *testing.T) {
	tests := []struct {
		name   string
		cookie string
		header string
		want   bool
	}{
		{"match", "tok123", "tok123", true},
		{"mismatch", "tok123", "other", false},
		{"missing header", "tok123", "", false},
		{"missing cookie", "", "tok123", false},
		{"both missing", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidCSRF(csrfRequest(tt.cookie, tt.header)); got != tt.want {
				t.Fatalf("ValidCSRF = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rl := NewRateLimiter(3, time.Minute)
	rl.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if !rl.Allow("1.2.3.4") {
			t.Fatalf("attempt %d: expected Allow to be true", i+1)
		}
	}
	if rl.Allow("1.2.3.4") {
		t.Fatal("4th attempt: expected Allow to be false")
	}

	// Advance past the window; the limiter should reset.
	now = now.Add(time.Minute + time.Second)
	if !rl.Allow("1.2.3.4") {
		t.Fatal("after window: expected Allow to be true")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "1.2.3.4:5678"
	if got := ClientIP(r); got != "1.2.3.4" {
		t.Fatalf("ClientIP = %q, want %q", got, "1.2.3.4")
	}
}
