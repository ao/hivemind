package router

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// whole is the route of a whole host with these endpoints.
func whole(eps ...string) []Route {
	if eps == nil {
		eps = []string{}
	}
	return []Route{{Endpoints: eps}}
}

func TestRoutingPath(t *testing.T) {
	for _, c := range []struct {
		target string // the request target as the client sent it
		want   string // the path matched on
		ok     bool
	}{
		{"/", "/", true},
		{"/api", "/api", true},
		{"/api/", "/api", true},
		{"/api/x?y=../z", "/api/x", true},
		{"//api//x/", "/api/x", true},
		{"/api;v=1/x", "/api;v=1/x", true},
		{"/%61pi/x", "/api/x", true},
		{"/caf%C3%A9", "/café", true},
		{"/a.b/..c/...", "/a.b/..c/...", true},
		{"*", "", true},
		// Dot segments, literal, encoded or with servlet parameters.
		{"/api/../admin", "", false},
		{"/admin/../api/x", "", false},
		{"/api/./x", "", false},
		{"/api/..", "", false},
		{"/api/%2e%2e/admin", "", false},
		{"/api/%2E/x", "", false},
		{"/api/.%2e/admin", "", false},
		{"/api/..;/admin", "", false},
		{"/api/.;x=1/y", "", false},
		{"/..", "", false},
		// Encoded slashes and backslashes, and backslashes.
		{"/api%2Fx", "", false},
		{"/api%2fx", "", false},
		{"/x/..%2Fapi", "", false},
		{"/api%5Cx", "", false},
		{"/api%5c..%5cadmin", "", false},
		{`/api\x`, "", false},
		{`/api/x\..\..\admin`, "", false},
		// Control characters, NUL included.
		{"/api%00/x", "", false},
		{"/api%0A", "", false},
		{"/api%7F", "", false},
	} {
		u, err := url.ParseRequestURI(c.target)
		if err != nil {
			t.Fatalf("%q: %v", c.target, err)
		}
		got, bad := routingPath(u)
		ok := bad == ""
		if got != c.want || ok != c.ok {
			t.Errorf("routingPath(%q) = %q, %v; want %q, %v", c.target, got, ok, c.want, c.ok)
		}
	}
}

func TestMatchLongestPrefixOnSegments(t *testing.T) {
	p := newProxy(slog.New(slog.DiscardHandler), nil)
	p.setTable(Table{"www.example.com": {
		{Path: "", Endpoints: []string{"10.0.0.1:1"}},
		{Path: "/api", Endpoints: []string{"10.0.0.2:1"}},
		{Path: "/api/v2", Endpoints: []string{"10.0.0.3:1"}},
		{Path: "/a.b", Endpoints: []string{"10.0.0.4:1"}},
	}, "api.example.com": {
		{Path: "/v1", Endpoints: []string{"10.0.0.5:1"}},
	}})
	for _, c := range []struct {
		host, target string
		want         string // the route's path, "-" for none, "400" for a refusal
	}{
		{"www.example.com", "/", ""},
		{"www.example.com", "/index.html", ""},
		{"www.example.com", "/api", "/api"},
		{"www.example.com", "/api/", "/api"},
		{"www.example.com", "/api/x", "/api"},
		{"www.example.com", "/api?x=1", "/api"},
		{"www.example.com", "/apix", ""},
		{"www.example.com", "/api.json", ""},
		{"www.example.com", "/API/x", ""},
		{"www.example.com", "/api/v2", "/api/v2"},
		{"www.example.com", "/api/v2/x", "/api/v2"},
		{"www.example.com", "/api/v20", "/api"},
		{"www.example.com", "//api/x", "/api"},
		{"www.example.com", "/%61pi/x", "/api"},
		{"www.example.com", "/api;/x", ""},
		{"www.example.com", "/a.b/c", "/a.b"},
		{"www.example.com", "/a.bc", ""},
		{"www.example.com", "*", ""},
		{"www.example.com", "/api/../x", "400"},
		{"www.example.com", "/x/../api/y", "400"},
		{"www.example.com", "/api%2Fx", "400"},
		{"www.example.com", "/x/%2e%2e/api", "400"},
		// A host with path routes and no whole host route takes nothing
		// else.
		{"api.example.com", "/v1/x", "/v1"},
		{"api.example.com", "/", "-"},
		{"api.example.com", "/v2", "-"},
		{"api.example.com", "*", "-"},
	} {
		u, err := url.ParseRequestURI(c.target)
		if err != nil {
			t.Fatal(err)
		}
		rt, bad := p.lookup(c.host).match(u)
		got := "-"
		switch {
		case bad != "":
			got = "400"
		case rt != nil:
			got = rt.path
		}
		if got != c.want {
			t.Errorf("%s%s went to %q, want %q", c.host, c.target, got, c.want)
		}
	}

	// A host without path routes takes every path, as before paths
	// existed, the ones a host with paths refuses among them.
	p.setTable(Table{"www.example.com": whole("10.0.0.1:1")})
	for _, target := range []string{"/", "/api/../x", "/api%2Fx", `/a\b`, "/a%00", "*"} {
		u, _ := url.ParseRequestURI(target)
		if rt, bad := p.lookup("www.example.com").match(u); bad != "" || rt == nil || rt.path != "" {
			t.Errorf("%s on a host without paths: %v, %q", target, rt, bad)
		}
	}
}

// rawGet sends target as it is over the router's https listener, with
// HTTP/1.1, and returns the status code and the body.
func (tr *testRouter) rawGet(t *testing.T, host, target string) (int, string) {
	t.Helper()
	conn, err := tls.Dial("tcp", tr.rt.HTTPSAddr(), &tls.Config{RootCAs: tr.pool, ServerName: host, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, host); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	_, _ = bufio.NewReader(resp.Body).WriteTo(&b)
	return resp.StatusCode, b.String()
}

// Two apps on one host: the path routes go to their own replicas, every
// other path to the whole host's, the path reaches the replica as the
// client sent it, and the metrics have the host and no path.
func TestTwoUpstreamsOnOneHost(t *testing.T) {
	web1, web2 := upstream(t, "web1"), upstream(t, "web2")
	api1, api2 := upstream(t, "api1"), upstream(t, "api2")
	tr := newTestRouter(t, Table{"www.example.com": {
		{Endpoints: []string{web1.Listener.Addr().String(), web2.Listener.Addr().String()}},
		{Path: "/api", Endpoints: []string{api1.Listener.Addr().String(), api2.Listener.Addr().String()}},
		{Path: "/status", Endpoints: []string{}},
	}})
	for _, h1 := range []bool{true, false} {
		c := tr.client(h1)
		for _, x := range []struct{ path, app string }{
			{"/", "web"}, {"/apix", "web"}, {"/index.html?a=b", "web"},
			{"/api", "api"}, {"/api/", "api"}, {"/api/users/7?x=%2F", "api"}, {"//api//users", "api"},
		} {
			names := map[string]bool{}
			for range 4 {
				req, _ := http.NewRequest(http.MethodGet, "https://www.example.com"+x.path, nil)
				resp, s, body := get(t, c, req)
				if resp.StatusCode != http.StatusOK || !strings.HasPrefix(s.Name, x.app) {
					t.Fatalf("h1=%v %s: %d from %q, want %s: %s", h1, x.path, resp.StatusCode, s.Name, x.app, body)
				}
				// No rewriting: the replica gets the client's request
				// target.
				if s.URI != x.path {
					t.Fatalf("h1=%v %s reached the replica as %q", h1, x.path, s.URI)
				}
				names[s.Name] = true
			}
			if len(names) != 2 {
				t.Fatalf("h1=%v %s: replies from %v, want both replicas of %s", h1, x.path, names, x.app)
			}
		}
	}
	// A path route with no Ready replica is 503, and the others still serve.
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/status/x", nil)
	if resp, _, _ := get(t, tr.client(false), req); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a path route without replicas: %d", resp.StatusCode)
	}
	// Paths that could be read as under another prefix are refused, by the
	// router, before any replica sees them.
	// The body names the one reason found.
	for target, reason := range map[string]string{
		"/api/../admin":      "a dot segment (. or .., or %2E)",
		"/x/../api/y":        "a dot segment (. or .., or %2E)",
		"/api/%2e%2e/web":    "a dot segment (. or .., or %2E)",
		"/api%2Fusers":       "an encoded slash (%2F)",
		"/api/..;/x":         "a dot segment (. or .., or %2E)",
		`/api/x\..\..\admin`: `a backslash (\ or %5C)`,
		"/api%5C..%5Cx":      `a backslash (\ or %5C)`,
	} {
		if code, body := tr.rawGet(t, "www.example.com", target); code != http.StatusBadRequest || strings.TrimSpace(body) != "bad request: the path has "+reason {
			t.Errorf("%s: %d %q, want 400 with %q", target, code, body, reason)
		}
	}
	if code, body := tr.rawGet(t, "www.example.com", "/api/users"); code != http.StatusOK || !strings.Contains(body, `"Name":"api`) {
		t.Fatalf("a raw request to /api/users: %d %s", code, body)
	}

	m := tr.metrics(t)
	if strings.Contains(m, "path") || strings.Contains(m, "/api") {
		t.Fatalf("the metrics name a path:\n%s", m)
	}
	for _, want := range []string{`{host="www.example.com",code="200"} 57`, `{host="www.example.com",code="503"} 1`,
		`{host="www.example.com",code="400"} 7`} {
		if !strings.Contains(m, "hivemind_router_requests_total"+want) {
			t.Errorf("metrics lack %s:\n%s", want, m)
		}
	}

	// The api app moves to /v2, and the table read changes nothing else.
	// The test router reads its table every 20 ms, so the new one is what
	// it reads, and it is set at once too.
	moved := Table{"www.example.com": {
		{Endpoints: []string{web1.Listener.Addr().String()}},
		{Path: "/v2", Endpoints: []string{api1.Listener.Addr().String()}},
	}}
	tr.setRoutes(moved, nil)
	tr.rt.SetTable(moved)
	// A read of the old table that was in flight may set it once more;
	// two reads after this one, the new table holds.
	for n := tr.readCount() + 2; tr.readCount() < n; {
		time.Sleep(5 * time.Millisecond)
	}
	for path, want := range map[string]string{"/api/x": "web1", "/v2": "api1", "/v2/x": "api1", "/": "web1"} {
		req, _ := http.NewRequest(http.MethodGet, "https://www.example.com"+path, nil)
		if resp, s, body := get(t, tr.client(false), req); resp.StatusCode != http.StatusOK || s.Name != want {
			t.Fatalf("%s after the table changed: %d from %q, want %s: %s", path, resp.StatusCode, s.Name, want, body)
		}
	}
}

// A host with path routes and no whole host route answers 404 for the
// rest, and keeps its certificate; a host without paths passes even the
// paths a host with paths refuses, as before.
func TestPathsWithoutAWholeHostRoute(t *testing.T) {
	api, web := upstream(t, "api"), upstream(t, "web")
	tr := newTestRouter(t, Table{
		"api.example.com": {{Path: "/v1", Endpoints: []string{api.Listener.Addr().String()}}},
		"www.example.com": whole(web.Listener.Addr().String()),
	})
	if code, body := tr.rawGet(t, "api.example.com", "/v1/x"); code != http.StatusOK || !strings.Contains(body, `"Name":"api"`) {
		t.Fatalf("/v1/x: %d %s", code, body)
	}
	for _, target := range []string{"/", "/v2", "/v1x"} {
		if code, body := tr.rawGet(t, "api.example.com", target); code != http.StatusNotFound || strings.TrimSpace(body) != "not found" {
			t.Errorf("%s: %d %s, want 404", target, code, body)
		}
	}
	if got := tr.rt.Managed(); len(got) != 2 {
		t.Fatalf("managed hosts %v", got)
	}
	// Go's own encoding of a path it cannot pass as it is, a backslash, is
	// as before.
	for target, uri := range map[string]string{"/x/../y": "/x/../y", "/a%2Fb": "/a%2Fb", `/a\b`: "/a%5Cb"} {
		code, body := tr.rawGet(t, "www.example.com", target)
		if code != http.StatusOK || !strings.Contains(body, `"URI":`+fmt.Sprintf("%q", uri)) {
			t.Errorf("%s on a host without paths: %d %s", target, code, body)
		}
	}
}

// routingPath gives the first reason it finds to refuse a path.
func TestRoutingPathReason(t *testing.T) {
	for target, want := range map[string]string{
		"/a/./b":    "a dot segment (. or .., or %2E)",
		"/a/%2E%2E": "a dot segment (. or .., or %2E)",
		"/a%00b":    "a control character",
		"/a%2fb":    "an encoded slash (%2F)",
		"/a%5cb":    "an encoded backslash (%5C)",
		`/a\b`:      `a backslash (\ or %5C)`,
		`/a%00\..`:  "a control character",
		"/a/b":      "",
		"*":         "",
	} {
		u, err := url.ParseRequestURI(target)
		if err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		if _, bad := routingPath(u); bad != want {
			t.Errorf("routingPath(%q): %q, want %q", target, bad, want)
		}
	}
}
