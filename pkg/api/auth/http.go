package auth

import (
	"net/http"
	"strings"
	"time"
)

// Now is the time source used for issuing and verifying sessions. It is a
// package variable so tests can control it.
var Now = time.Now

const bearerPrefix = "Bearer "

// Authorized reports whether the request carries either a valid bearer token
// (for programmatic clients and CI) or a valid session cookie (for the browser UI).
func (a *Authenticator) Authorized(r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, bearerPrefix) {
		if a.CheckToken(strings.TrimPrefix(h, bearerPrefix)) {
			return true
		}
	}
	if c, err := r.Cookie(SessionCookieName); err == nil {
		if a.Verify(c.Value, Now()) {
			return true
		}
	}
	return false
}

// RequireAPI wraps an API handler, returning 401 for unauthorized requests.
func (a *Authenticator) RequireAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUI wraps a UI handler, redirecting unauthorized browsers to /login.
func (a *Authenticator) RequireUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authorized(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SetSession writes a fresh session cookie. secure marks the cookie Secure (use
// when served over TLS).
func (a *Authenticator) SetSession(w http.ResponseWriter, secure bool) {
	now := Now()
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    a.Issue(now),
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  now.Add(a.ttl),
	})
}

// ClearSession expires the session cookie.
func (a *Authenticator) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}
