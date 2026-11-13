package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/ratelimit"
	"github.com/ao/hivemind/internal/store"
)

// sessionEnv is an API with sessions of a chosen length and its own login
// limiter.
type sessionEnv struct {
	h     http.Handler
	store *store.Store
	token string
}

func newSessionEnv(t *testing.T, ttl time.Duration, lim *ratelimit.FailLimiter) *sessionEnv {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertNode(context.Background(), store.Node{ID: testNode, Name: "test", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sessions, err := auth.NewSessions(ttl)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.NewAuthenticator(auth.HashToken(tok), sessions)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, Runtime: &fakeRuntime{}, NodeID: testNode, Auth: a,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), LoginLimiter: lim})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.CloseStreams)
	return &sessionEnv{h: srv.Handler(), store: st, token: tok}
}

// req sends one request. hdr holds extra headers; cookie, if set, is sent.
func (e *sessionEnv) req(method, path, body string, hdr map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

// login posts the bearer token to /v1/session and returns the session cookie
// and the CSRF token.
func (e *sessionEnv) login(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	rec := e.req("POST", "/v1/session", "", map[string]string{"Authorization": "Bearer " + e.token}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.CSRFToken == "" {
		t.Fatalf("login body %q: %v", rec.Body.String(), err)
	}
	cookie := sessionCookieOf(t, rec)
	return cookie, body.CSRFToken
}

func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in %v", auth.SessionCookieName, rec.Header().Values("Set-Cookie"))
	return nil
}

const webSpec = `{"name":"web","image":"nginx:alpine","replicas":1}`

func TestSessionLoginCookieAndCSRF(t *testing.T) {
	e := newSessionEnv(t, time.Hour, nil)
	cookie, csrf := e.login(t)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Secure {
		t.Fatalf("cookie over plain HTTP: %+v", cookie)
	}
	if cookie.MaxAge != int(time.Hour.Seconds()) {
		t.Fatalf("cookie Max-Age %d, want %d", cookie.MaxAge, int(time.Hour.Seconds()))
	}

	// A cookie authenticates a GET without the bearer token.
	rec := e.req("GET", "/v1/status", "", nil, cookie)
	wantStatus(t, rec, http.StatusOK)

	// An unsafe request needs the CSRF token: 403 without it, and nothing
	// is written.
	rec = e.req("PUT", "/v1/apps/web", webSpec, nil, cookie)
	wantStatus(t, rec, http.StatusForbidden)
	if apps, _ := e.store.ListApps(context.Background()); len(apps) != 0 {
		t.Fatalf("a cookie PUT without CSRF created %d apps", len(apps))
	}
	rec = e.req("PUT", "/v1/apps/web", webSpec, map[string]string{auth.CSRFHeader: "wrong"}, cookie)
	wantStatus(t, rec, http.StatusForbidden)
	rec = e.req("PUT", "/v1/apps/web", webSpec, map[string]string{auth.CSRFHeader: csrf}, cookie)
	wantStatus(t, rec, http.StatusCreated)
	rec = e.req("POST", "/v1/apps/web/scale", `{"replicas":2}`, map[string]string{auth.CSRFHeader: csrf}, cookie)
	wantStatus(t, rec, http.StatusOK)

	// A form post carries the token in the csrf_token field.
	rec = e.req("POST", "/v1/join-tokens", "", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, cookie)
	wantStatus(t, rec, http.StatusForbidden)

	// A cookie cannot mint a second session, even with its CSRF token.
	rec = e.req("POST", "/v1/session", "", map[string]string{auth.CSRFHeader: csrf}, cookie)
	wantStatus(t, rec, http.StatusUnauthorized)

	// Logout needs the CSRF token too.
	rec = e.req("DELETE", "/v1/session", "", nil, cookie)
	wantStatus(t, rec, http.StatusForbidden)
	if rec := e.req("GET", "/v1/status", "", nil, cookie); rec.Code != http.StatusOK {
		t.Fatalf("a refused logout ended the session: %d", rec.Code)
	}
	rec = e.req("DELETE", "/v1/session", "", map[string]string{auth.CSRFHeader: csrf}, cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body.String())
	}
	if cleared := sessionCookieOf(t, rec); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}
	// The old cookie no longer works.
	rec = e.req("GET", "/v1/status", "", nil, cookie)
	wantStatus(t, rec, http.StatusUnauthorized)
}

func TestSessionLoginNeedsTheToken(t *testing.T) {
	e := newSessionEnv(t, time.Hour, nil)
	other, _ := auth.NewToken()
	for _, hdr := range []map[string]string{nil, {"Authorization": "Bearer " + other}, {"Authorization": "Basic " + e.token}} {
		rec := e.req("POST", "/v1/session", "", hdr, nil)
		wantStatus(t, rec, http.StatusUnauthorized)
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("a refused login set a cookie: %v", rec.Header().Values("Set-Cookie"))
		}
	}
}

// A bad bearer header is refused even with a live cookie beside it.
func TestSessionBadHeaderIsNotRescuedByCookie(t *testing.T) {
	e := newSessionEnv(t, time.Hour, nil)
	cookie, _ := e.login(t)
	rec := e.req("GET", "/v1/status", "", map[string]string{"Authorization": "Bearer nope"}, cookie)
	wantStatus(t, rec, http.StatusUnauthorized)
}

func TestSessionExpires(t *testing.T) {
	e := newSessionEnv(t, 200*time.Millisecond, nil)
	cookie, _ := e.login(t)
	wantStatus(t, e.req("GET", "/v1/status", "", nil, cookie), http.StatusOK)
	time.Sleep(300 * time.Millisecond)
	wantStatus(t, e.req("GET", "/v1/status", "", nil, cookie), http.StatusUnauthorized)
}

func TestSessionCookieIsSecureOverTLS(t *testing.T) {
	e := newSessionEnv(t, time.Hour, nil)
	r := httptest.NewRequest("POST", "https://127.0.0.1:4483/v1/session", nil)
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
	r.Header.Set("Authorization", "Bearer "+e.token)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("login over TLS: %d", rec.Code)
	}
	if c := sessionCookieOf(t, rec); !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie over TLS: %+v", c)
	}
}

func TestLoginRateLimit(t *testing.T) {
	lim := ratelimit.New(3, time.Hour)
	e := newSessionEnv(t, time.Hour, lim)
	bad := map[string]string{"Authorization": "Bearer hm_" + strings.Repeat("A", 43)}
	for i := range 3 {
		if rec := e.req("POST", "/v1/session", "", bad, nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failed login %d: %d", i+1, rec.Code)
		}
	}
	// Over the limit, even the right token is refused from this address.
	rec := e.req("POST", "/v1/session", "", map[string]string{"Authorization": "Bearer " + e.token}, nil)
	wantStatus(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a rate-limited login set a cookie")
	}
	// Another address is not affected, and other routes are not limited.
	r := httptest.NewRequest("POST", "/v1/session", nil)
	r.RemoteAddr = "198.51.100.7:1234"
	r.Header.Set("Authorization", "Bearer "+e.token)
	rec2 := httptest.NewRecorder()
	e.h.ServeHTTP(rec2, r)
	if rec2.Code != http.StatusOK {
		t.Fatalf("login from another address: %d", rec2.Code)
	}
	wantStatus(t, e.req("GET", "/v1/status", "", map[string]string{"Authorization": "Bearer " + e.token}, nil), http.StatusOK)
}

// Successful logins and logouts do not count against the limit.
func TestLoginRateLimitCountsOnlyFailures(t *testing.T) {
	e := newSessionEnv(t, time.Hour, ratelimit.New(1, time.Hour))
	for range 5 {
		cookie, csrf := e.login(t)
		if rec := e.req("DELETE", "/v1/session", "", map[string]string{auth.CSRFHeader: csrf}, cookie); rec.Code != http.StatusNoContent {
			t.Fatalf("logout: %d", rec.Code)
		}
	}
}
