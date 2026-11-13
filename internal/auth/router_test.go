package auth

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

var (
	routeEndMarker = regexp.MustCompile(`\{\$\}$`)
	routeWildcard  = regexp.MustCompile(`\{[^}]*\}`)
)

// probeRequest turns a ServeMux pattern into a concrete request: the method
// from the pattern (GET if there is none), and the path with each wildcard
// replaced by a placeholder. This is the pattern a future router's test
// should use.
func probeRequest(pattern string) (method, target string) {
	method = http.MethodGet
	if m, p, ok := strings.Cut(pattern, " "); ok {
		method, pattern = m, p
	}
	target = routeEndMarker.ReplaceAllString(pattern, "")
	target = routeWildcard.ReplaceAllString(target, "probe")
	return method, target
}

// TestEveryRouteRequiresAuthExceptHealthz registers the routes on a Router,
// wraps them in the middleware, and sends each route a request with no
// credentials. Every route except GET /healthz must answer 401. A route that
// answers 200 is an unprotected route. The test then sends each non-health
// route the admin token, to check that the 401s above come from the gate and
// not from a route that never matches.
func TestEveryRouteRequiresAuthExceptHealthz(t *testing.T) {
	f := newFixture(t)
	router := f.router()
	patterns := router.Patterns()
	if len(patterns) < 5 {
		t.Fatalf("router lists %d patterns; the enumeration is not seeing the routes", len(patterns))
	}
	h := f.auth.Middleware(router)

	for _, p := range patterns {
		method, target := probeRequest(p)
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
			if method == http.MethodGet && target == "/healthz" {
				if rec.Code != http.StatusOK {
					t.Fatalf("GET /healthz without credentials: status %d, want 200", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s without credentials: status %d, want 401", method, target, rec.Code)
			}
			if rec.Body.String() != bodyUnauthorized {
				t.Fatalf("401 body %q, want %q", rec.Body.String(), bodyUnauthorized)
			}

			// The same route with the token reaches its handler. Some handlers
			// answer 204 or 201, so the check is that the gate lets it through.
			authed := httptest.NewRequest(method, target, nil)
			authed.Header.Set("Authorization", "Bearer "+f.token)
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, authed)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Fatalf("%s %s with the token: status %d, the gate rejected it", method, target, rec.Code)
			}
		})
	}
}

// TestUnknownPathsAreNotRevealed checks that a path with no route gets the
// same 401 as a real one, so the gate does not reveal which paths exist.
func TestUnknownPathsAreNotRevealed(t *testing.T) {
	f := newFixture(t)
	h := f.auth.Middleware(f.router())
	for _, target := range []string{"/nope", "/api/apps/web/unknown", "/", "/api"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without credentials: status %d, want 401", target, rec.Code)
		}
	}
}

func TestProbeRequest(t *testing.T) {
	cases := map[string][2]string{
		"GET /healthz":                 {"GET", "/healthz"},
		"/ui/":                         {"GET", "/ui/"},
		"PUT /api/apps/{name}":         {"PUT", "/api/apps/probe"},
		"GET /files/{path...}":         {"GET", "/files/probe"},
		"GET /exact/{$}":               {"GET", "/exact/"},
		"DELETE /api/apps/{name}/logs": {"DELETE", "/api/apps/probe/logs"},
	}
	for in, want := range cases {
		m, tgt := probeRequest(in)
		if m != want[0] || tgt != want[1] {
			t.Errorf("probeRequest(%q) = (%q, %q), want (%q, %q)", in, m, tgt, want[0], want[1])
		}
	}
}
