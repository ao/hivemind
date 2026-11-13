package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fixture is an admin token, a session store and an Authenticator on a clock
// that the test controls.
type fixture struct {
	token    string
	clock    *fakeClock
	sessions *Sessions
	auth     *Authenticator
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	s, clock := newTestSessions(t, time.Hour)
	a, err := NewAuthenticator(HashToken(tok), s)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{token: tok, clock: clock, sessions: s, auth: a}
}

// router registers the routes the tests use. Each handler answers 200 with
// its name, so a test can see that a request reached it.
func (f *fixture) router() *Router {
	r := NewRouter()
	named := func(name string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, name)
		}
	}
	r.HandleFunc("GET /healthz", named("healthz"))
	r.HandleFunc("GET /api/apps", named("list"))
	r.HandleFunc("POST /api/apps", named("create"))
	r.HandleFunc("PUT /api/apps/{name}", named("put"))
	r.HandleFunc("PATCH /api/apps/{name}", named("patch"))
	r.HandleFunc("DELETE /api/apps/{name}", named("delete"))
	r.HandleFunc("GET /ui/", named("ui"))
	r.Handle("POST /login", f.auth.LoginHandler())
	r.Handle("POST /logout", f.auth.LogoutHandler())
	return r
}

func (f *fixture) handler() http.Handler { return f.auth.Middleware(f.router()) }

// cookieRequest returns a request that carries a new live session cookie,
// and the session's CSRF token.
func cookieRequest(t *testing.T, f *fixture, method, path, body string) (*http.Request, string) {
	t.Helper()
	id, csrf, err := f.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: id})
	return r, csrf
}

func withHeader(r *http.Request, key, value string) *http.Request {
	r.Header.Set(key, value)
	return r
}

// assertNoSecretLeak fails if the admin token appears anywhere in the response.
func assertNoSecretLeak(t *testing.T, f *fixture, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), f.token) {
		t.Fatal("response body contains the admin token")
	}
	for k, vs := range rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, f.token) {
				t.Fatalf("response header %s contains the admin token", k)
			}
		}
	}
}

func TestMiddlewareTable(t *testing.T) {
	const (
		get   = http.MethodGet
		post  = http.MethodPost
		put   = http.MethodPut
		patch = http.MethodPatch
		del   = http.MethodDelete
	)
	type build func(t *testing.T, f *fixture) *http.Request
	bare := func(method, path string) build {
		return func(*testing.T, *fixture) *http.Request {
			return httptest.NewRequest(method, path, nil)
		}
	}
	bearer := func(method, path, value string) build {
		return func(_ *testing.T, f *fixture) *http.Request {
			return withHeader(httptest.NewRequest(method, path, nil), "Authorization", strings.ReplaceAll(value, "TOKEN", f.token))
		}
	}
	cookie := func(method, path string, expire bool, csrf func(string) string) build {
		return func(t *testing.T, f *fixture) *http.Request {
			r, token := cookieRequest(t, f, method, path, "")
			if expire {
				f.clock.t = f.clock.t.Add(2 * time.Hour)
			}
			if csrf != nil {
				r.Header.Set(CSRFHeader, csrf(token))
			}
			return r
		}
	}
	noCSRF := func(string) string { return "" }
	wrongCSRF := func(string) string { return "wrong-token" }
	rightCSRF := func(tok string) string { return tok }

	cases := []struct {
		name  string
		build build
		want  int
	}{
		{"no credentials", bare(get, "/api/apps"), http.StatusUnauthorized},
		{"empty Authorization header", bearer(get, "/api/apps", ""), http.StatusUnauthorized},
		{"Basic scheme", bearer(get, "/api/apps", "Basic dXNlcjpwYXNz"), http.StatusUnauthorized},
		{"Bearer with no token", bearer(get, "/api/apps", "Bearer "), http.StatusUnauthorized},
		{"wrong token of the right shape", bearer(get, "/api/apps", "Bearer hm_"+strings.Repeat("A", 43)), http.StatusUnauthorized},
		{"right token with a suffix", bearer(get, "/api/apps", "Bearer TOKENx"), http.StatusUnauthorized},
		{"right token", bearer(get, "/api/apps", "Bearer TOKEN"), http.StatusOK},
		{"right token, lower-case scheme", bearer(get, "/api/apps", "bearer TOKEN"), http.StatusOK},
		{"right token, unsafe method, no CSRF needed", bearer(post, "/api/apps", "Bearer TOKEN"), http.StatusOK},
		{"right token, PUT", bearer(put, "/api/apps/web", "Bearer TOKEN"), http.StatusOK},
		{"right token, DELETE", bearer(del, "/api/apps/web", "Bearer TOKEN"), http.StatusOK},
		{"wrong bearer with a live cookie", func(t *testing.T, f *fixture) *http.Request {
			r, _ := cookieRequest(t, f, get, "/api/apps", "")
			return withHeader(r, "Authorization", "Bearer hm_"+strings.Repeat("A", 43))
		}, http.StatusUnauthorized},
		{"two Authorization headers, the right one first", func(_ *testing.T, f *fixture) *http.Request {
			r := httptest.NewRequest(get, "/api/apps", nil)
			r.Header.Add("Authorization", "Bearer "+f.token)
			r.Header.Add("Authorization", "Bearer "+f.token)
			return r
		}, http.StatusUnauthorized},
		{"live session cookie, GET", cookie(get, "/api/apps", false, nil), http.StatusOK},
		{"expired session cookie, GET", cookie(get, "/api/apps", true, nil), http.StatusUnauthorized},
		{"unknown session cookie", func(*testing.T, *fixture) *http.Request {
			r := httptest.NewRequest(get, "/api/apps", nil)
			r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: strings.Repeat("x", 43)})
			return r
		}, http.StatusUnauthorized},
		{"cookie under another name", func(*testing.T, *fixture) *http.Request {
			r := httptest.NewRequest(get, "/api/apps", nil)
			r.AddCookie(&http.Cookie{Name: "session", Value: strings.Repeat("x", 43)})
			return r
		}, http.StatusUnauthorized},
		{"live cookie, GET without CSRF", cookie(get, "/ui/", false, noCSRF), http.StatusOK},
		{"live cookie, POST without CSRF", cookie(post, "/api/apps", false, noCSRF), http.StatusForbidden},
		{"live cookie, POST with wrong CSRF", cookie(post, "/api/apps", false, wrongCSRF), http.StatusForbidden},
		{"live cookie, POST with right CSRF", cookie(post, "/api/apps", false, rightCSRF), http.StatusOK},
		{"live cookie, PUT without CSRF", cookie(put, "/api/apps/web", false, noCSRF), http.StatusForbidden},
		{"live cookie, PATCH without CSRF", cookie(patch, "/api/apps/web", false, noCSRF), http.StatusForbidden},
		{"live cookie, DELETE without CSRF", cookie(del, "/api/apps/web", false, noCSRF), http.StatusForbidden},
		{"live cookie, DELETE with right CSRF", cookie(del, "/api/apps/web", false, rightCSRF), http.StatusOK},
		{"expired cookie, POST with right CSRF", cookie(post, "/api/apps", true, rightCSRF), http.StatusUnauthorized},
		{"CSRF token of another session", func(t *testing.T, f *fixture) *http.Request {
			r, _ := cookieRequest(t, f, post, "/api/apps", "")
			_, other := cookieRequest(t, f, post, "/api/apps", "")
			r.Header.Set(CSRFHeader, other)
			return r
		}, http.StatusForbidden},
		{"CSRF form field, right token", func(t *testing.T, f *fixture) *http.Request {
			r, csrf := cookieRequest(t, f, post, "/api/apps", "")
			r.Body = io.NopCloser(strings.NewReader(url.Values{csrfField: {csrf}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, http.StatusOK},
		{"CSRF form field, wrong token", func(t *testing.T, f *fixture) *http.Request {
			r, _ := cookieRequest(t, f, post, "/api/apps", "")
			r.Body = io.NopCloser(strings.NewReader(url.Values{csrfField: {"nope"}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r
		}, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			rec := httptest.NewRecorder()
			f.handler().ServeHTTP(rec, c.build(t, f))
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d; body %q", rec.Code, c.want, rec.Body.String())
			}
			assertNoSecretLeak(t, f, rec)
			switch c.want {
			case http.StatusUnauthorized:
				if rec.Body.String() != bodyUnauthorized {
					t.Fatalf("401 body %q, want %q", rec.Body.String(), bodyUnauthorized)
				}
				if rec.Header().Get("WWW-Authenticate") == "" {
					t.Fatal("401 without a WWW-Authenticate header")
				}
			case http.StatusForbidden:
				if rec.Body.String() != bodyCSRF {
					t.Fatalf("403 body %q, want %q", rec.Body.String(), bodyCSRF)
				}
			}
		})
	}
}

// TestHealthzExemptIsExact checks that only GET /healthz, matched exactly,
// skips authentication. HEAD, POST, a trailing slash and other spellings
// are not exempt.
func TestHealthzExemptIsExact(t *testing.T) {
	cases := []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/healthz?probe=1", http.StatusOK},
		{http.MethodHead, "/healthz", http.StatusUnauthorized},
		{http.MethodPost, "/healthz", http.StatusUnauthorized},
		{http.MethodPut, "/healthz", http.StatusUnauthorized},
		{http.MethodGet, "/healthz/", http.StatusUnauthorized},
		{http.MethodGet, "/HEALTHZ", http.StatusUnauthorized},
		{http.MethodGet, "/healthz/../api/apps", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			f := newFixture(t)
			rec := httptest.NewRecorder()
			f.handler().ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
			assertNoSecretLeak(t, f, rec)
		})
	}
}

// sentResponse is what send returns: the status, headers, body and cookies.
type sentResponse struct {
	status  int
	header  http.Header
	body    string
	cookies []*http.Cookie
}

func send(t *testing.T, c *http.Client, method, u string, hdr map[string]string, cookie *http.Cookie) sentResponse {
	t.Helper()
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return sentResponse{status: resp.StatusCode, header: resp.Header, body: string(b), cookies: resp.Cookies()}
}

func findCookie(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func expectStatus(t *testing.T, r sentResponse, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d; body %q", r.status, want, r.body)
	}
}

// TestLoginLogoutFlow runs the UI flow over real HTTP: login with the token,
// use the session, CSRF on unsafe requests, logout.
func TestLoginLogoutFlow(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := srv.Client()
	login := srv.URL + "/login"
	bearer := map[string]string{"Authorization": "Bearer " + f.token}

	// Login is itself gated: it needs the token as a bearer header.
	expectStatus(t, send(t, c, http.MethodPost, login, nil, nil), http.StatusUnauthorized)
	expectStatus(t, send(t, c, http.MethodPost, login, map[string]string{"Authorization": "Bearer hm_" + strings.Repeat("A", 43)}, nil), http.StatusUnauthorized)
	liveID, liveCSRF, err := f.sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	// A session cookie cannot mint a new session. Without the CSRF token the
	// middleware refuses first; with it, the login handler still wants the
	// bearer token.
	liveCookie := &http.Cookie{Name: SessionCookieName, Value: liveID}
	expectStatus(t, send(t, c, http.MethodPost, login, nil, liveCookie), http.StatusForbidden)
	expectStatus(t, send(t, c, http.MethodPost, login, map[string]string{CSRFHeader: liveCSRF}, liveCookie), http.StatusUnauthorized)

	// Login with the token.
	r := send(t, c, http.MethodPost, login, bearer, nil)
	expectStatus(t, r, http.StatusOK)
	if strings.Contains(r.body, f.token) {
		t.Fatal("login response contains the admin token")
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.CSRFToken == "" {
		t.Fatalf("login body %q: %v", r.body, err)
	}
	sc := findCookie(r.cookies, SessionCookieName)
	if sc == nil || sc.Value == "" {
		t.Fatal("login did not set the session cookie")
	}
	if !sc.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if sc.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want Strict", sc.SameSite)
	}
	if sc.Path != "/" {
		t.Errorf("session cookie Path = %q, want /", sc.Path)
	}
	if sc.Secure {
		t.Error("session cookie is Secure over plain HTTP; loopback HTTP would not work")
	}
	if sc.MaxAge <= 0 {
		t.Errorf("session cookie MaxAge = %d, want a positive lifetime", sc.MaxAge)
	}

	// The session works for reads, and unsafe requests need the CSRF token.
	expectStatus(t, send(t, c, http.MethodGet, srv.URL+"/api/apps", nil, sc), http.StatusOK)
	expectStatus(t, send(t, c, http.MethodPost, srv.URL+"/api/apps", nil, sc), http.StatusForbidden)
	expectStatus(t, send(t, c, http.MethodPost, srv.URL+"/api/apps", map[string]string{CSRFHeader: "wrong"}, sc), http.StatusForbidden)
	expectStatus(t, send(t, c, http.MethodPost, srv.URL+"/api/apps", map[string]string{CSRFHeader: out.CSRFToken}, sc), http.StatusOK)

	// Logout is a POST too, so it needs the CSRF token. A failed logout keeps
	// the session.
	expectStatus(t, send(t, c, http.MethodPost, srv.URL+"/logout", nil, sc), http.StatusForbidden)
	expectStatus(t, send(t, c, http.MethodGet, srv.URL+"/api/apps", nil, sc), http.StatusOK)

	r = send(t, c, http.MethodPost, srv.URL+"/logout", map[string]string{CSRFHeader: out.CSRFToken}, sc)
	expectStatus(t, r, http.StatusNoContent)
	cleared := findCookie(r.cookies, SessionCookieName)
	if cleared == nil || cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Fatalf("logout did not clear the cookie: %+v", cleared)
	}

	// The old cookie no longer works.
	expectStatus(t, send(t, c, http.MethodGet, srv.URL+"/api/apps", nil, sc), http.StatusUnauthorized)
	if _, ok := f.sessions.Lookup(sc.Value); ok {
		t.Fatal("logout left the session in the store")
	}
}

// TestLoginSetsSecureCookieOverTLS checks that the cookie gets the Secure
// flag when the request arrives over TLS.
func TestLoginSetsSecureCookieOverTLS(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()
	r := send(t, srv.Client(), http.MethodPost, srv.URL+"/login", map[string]string{"Authorization": "Bearer " + f.token}, nil)
	expectStatus(t, r, http.StatusOK)
	sc := findCookie(r.cookies, SessionCookieName)
	if sc == nil || !sc.Secure || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode {
		t.Fatalf("TLS session cookie flags wrong: %+v", sc)
	}
}

// TestSameSecretUsesConstantTimeCompare checks the code structure. The CSRF
// check must go through sameSecret, and sameSecret must call
// subtle.ConstantTimeCompare and must not compare with bytes.Equal.
func TestSameSecretUsesConstantTimeCompare(t *testing.T) {
	csrf := findFunc(t, "middleware.go", "csrfMatches")
	if !callsNamed(csrf, "sameSecret") {
		t.Fatal("csrfMatches does not compare through sameSecret")
	}
	fn := findFunc(t, "middleware.go", "sameSecret")
	if !callsNamed(fn, "ConstantTimeCompare") {
		t.Fatal("sameSecret does not call subtle.ConstantTimeCompare")
	}
	if callsNamed(fn, "Equal") {
		t.Fatal("sameSecret calls an Equal function, which is not constant-time")
	}
}
