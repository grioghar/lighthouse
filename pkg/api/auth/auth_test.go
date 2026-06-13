package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckToken(t *testing.T) {
	a := New("s3cr3t", "")
	if !a.CheckToken("s3cr3t") {
		t.Fatal("expected matching token to pass")
	}
	if a.CheckToken("wrong") {
		t.Fatal("expected wrong token to fail")
	}
	if a.CheckToken("") {
		t.Fatal("expected empty token to fail")
	}
	if New("", "").CheckToken("") {
		t.Fatal("empty configured token must never match")
	}
}

func TestIssueVerify(t *testing.T) {
	a := New("token", "signing-key")
	now := time.Unix(1_700_000_000, 0)

	v := a.Issue(now)
	if !a.Verify(v, now) {
		t.Fatal("freshly issued session should verify")
	}
	if a.Verify(v, now.Add(DefaultTTL+time.Second)) {
		t.Fatal("expired session should not verify")
	}
	if a.Verify(v+"x", now) {
		t.Fatal("tampered session should not verify")
	}
	// A different key must reject the signature.
	if New("token", "other-key").Verify(v, now) {
		t.Fatal("session signed with a different key should not verify")
	}
}

func TestAuthorized(t *testing.T) {
	a := New("tok", "key")
	Now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	defer func() { Now = time.Now }()

	// Valid bearer token.
	r := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	r.Header.Set("Authorization", "Bearer tok")
	if !a.Authorized(r) {
		t.Fatal("valid bearer token should authorize")
	}

	// Valid session cookie.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: a.Issue(Now())})
	if !a.Authorized(r) {
		t.Fatal("valid session cookie should authorize")
	}

	// Nothing.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	if a.Authorized(r) {
		t.Fatal("request without credentials should not authorize")
	}
}

func TestRequireAPI(t *testing.T) {
	a := New("tok", "key")
	h := a.RequireAPI(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	r.Header.Set("Authorization", "Bearer tok")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", rec.Code)
	}
}
