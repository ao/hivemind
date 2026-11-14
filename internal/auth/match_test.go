package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterMatch(t *testing.T) {
	r := NewRouter()
	ok := func(http.ResponseWriter, *http.Request) {}
	r.HandleFunc("GET /v1/apps", ok)
	r.HandleFunc("GET /v1/apps/{name}", ok)
	r.HandleFunc("DELETE /v1/apps/{name}", ok)
	cases := []struct{ method, path, want string }{
		{"GET", "/v1/apps", "GET /v1/apps"},
		{"GET", "/v1/apps/web", "GET /v1/apps/{name}"},
		{"HEAD", "/v1/apps/web", "GET /v1/apps/{name}"},
		{"DELETE", "/v1/apps/other", "DELETE /v1/apps/{name}"},
		{"POST", "/v1/apps/web", ""},
		{"GET", "/v1/nope", ""},
		// The mux redirects these to the clean path, and Match names the
		// pattern of that path. It is still a registered pattern.
		{"GET", "/v1//apps/web", "GET /v1/apps/{name}"},
		{"GET", "/v1/apps/../apps/web", "GET /v1/apps/{name}"},
		{"GET", "/v1//nope", ""},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "http://h"+c.path, nil)
		if got := r.Match(req); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.method, c.path, got, c.want)
		}
	}
}
