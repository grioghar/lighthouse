package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRFCookieName is the cookie holding the CSRF token (readable by JS so the UI
// can echo it back in the X-CSRF-Token header).
const CSRFCookieName = "lighthouse_csrf"

// CSRFHeaderName is the request header carrying the CSRF token.
const CSRFHeaderName = "X-CSRF-Token"

// NewCSRFToken returns a new random URL-safe token.
func NewCSRFToken() string {
	b := make([]byte, 32)
	// crypto/rand.Read never returns an error on supported platforms.
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// SetCSRFCookie writes the CSRF cookie. Unlike the session cookie it is NOT
// HttpOnly, so the UI's JavaScript can read it and echo it back in the
// X-CSRF-Token header for the double-submit check.
func SetCSRFCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ValidCSRF reports whether the request's CSRF header matches its CSRF cookie,
// using a constant-time comparison. A missing cookie or missing header is
// treated as invalid.
func ValidCSRF(r *http.Request) bool {
	c, err := r.Cookie(CSRFCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	header := r.Header.Get(CSRFHeaderName)
	if header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(header), []byte(c.Value)) == 1
}
