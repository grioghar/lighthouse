// Package auth provides token-backed session cookies for the Lighthouse web UI.
//
// There is no user database: the existing HTTP API token (--http-api-token) is the
// single credential. A successful login exchanges that token for a signed, httpOnly
// cookie so a browser can stay authenticated without resending the token.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// SessionCookieName is the name of the cookie holding the signed session value.
const SessionCookieName = "lighthouse_session"

// DefaultTTL is how long an issued session remains valid.
const DefaultTTL = 12 * time.Hour

// Authenticator validates the API token and issues/verifies signed session values.
//
// The zero value is not usable; construct one with New.
type Authenticator struct {
	token string
	key   []byte
	ttl   time.Duration
}

// New returns an Authenticator that accepts the given API token. If secret is
// empty a random per-process signing key is generated, which is fine for a
// single instance; supply a stable secret to keep sessions valid across restarts
// or multiple replicas.
func New(token, secret string) *Authenticator {
	key := []byte(secret)
	if len(key) == 0 {
		key = make([]byte, 32)
		// crypto/rand.Read never returns an error on supported platforms.
		_, _ = rand.Read(key)
	}
	return &Authenticator{token: token, key: key, ttl: DefaultTTL}
}

// TTL returns the session lifetime.
func (a *Authenticator) TTL() time.Duration { return a.ttl }

// CheckToken reports, in constant time, whether the supplied token matches the
// configured API token. An empty configured or supplied token never matches.
func (a *Authenticator) CheckToken(token string) bool {
	if a.token == "" || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(a.token)) == 1
}

// Issue returns a signed session value that expires at now+TTL.
func (a *Authenticator) Issue(now time.Time) string {
	payload := strconv.FormatInt(now.Add(a.ttl).Unix(), 10)
	return payload + "." + a.sign(payload)
}

// Verify reports whether value carries a valid signature and has not expired.
func (a *Authenticator) Verify(value string, now time.Time) bool {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(a.sign(payload))) != 1 {
		return false
	}
	exp, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return false
	}
	return now.Unix() < exp
}

func (a *Authenticator) sign(payload string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
