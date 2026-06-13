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

// bearerAuthorized reports whether the request carries a valid bearer token.
func (a *Authenticator) bearerAuthorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, bearerPrefix) && a.CheckToken(strings.TrimPrefix(h, bearerPrefix))
}

// cookieAuthorized reports whether the request carries a valid session cookie.
func (a *Authenticator) cookieAuthorized(r *http.Request) bool {
	c, err := r.Cookie(SessionCookieName)
	return err == nil && a.Verify(c.Value, Now())
}

// Authorized reports whether the request carries either a valid bearer token
// (for programmatic clients and CI) or a valid session cookie (for the browser UI).
func (a *Authenticator) Authorized(r *http.Request) bool {
	return a.bearerAuthorized(r) || a.cookieAuthorized(r)
}

func isUnsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// RequireAPI wraps an API handler, returning 401 for unauthorized requests. For
// state-changing methods authenticated by the session cookie (i.e. a browser),
// it also requires a valid CSRF token; bearer-token clients are exempt (they are
// not subject to ambient-credential CSRF).
func (a *Authenticator) RequireAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if isUnsafeMethod(r.Method) && !a.bearerAuthorized(r) && !ValidCSRF(r) {
			http.Error(w, `{"error":"missing or invalid CSRF token"}`, http.StatusForbidden)
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
