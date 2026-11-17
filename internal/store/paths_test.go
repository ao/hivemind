package store

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
)

// withPath is an app with one route of host, under path, to port 80.
func withPath(name, host, path string) spec.App {
	a := routed(name, 1)
	a.Routes = []spec.Route{{Host: host, Path: path, Port: 80}}
	return a
}

// routeKeys lists the table's routes as host+path=app.
func routeKeys(t *testing.T, s *Store) []string {
	t.Helper()
	rt, err := s.RouteTable(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rt {
		out = append(out, r.Key().String()+"="+r.App)
	}
	return out
}

func TestPathRoutesShareAHost(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, routed("web", 1, "www.example.com"))
	// Another app takes a path of the host, and a third a longer one.
	mustPut(t, s, withPath("api", "www.example.com", "/api"))
	mustPut(t, s, withPath("v2", "www.example.com", "/api/v2"))
	// One app can have several routes on one host.
	admin := withPath("admin", "www.example.com", "/admin")
	admin.Routes = append(admin.Routes, spec.Route{Host: "www.example.com", Path: "/internal", Port: 81})
	mustPut(t, s, admin)
	want := []string{"www.example.com=web", "www.example.com/admin=admin", "www.example.com/api=api",
		"www.example.com/api/v2=v2", "www.example.com/internal=admin"}
	if got := routeKeys(t, s); !slices.Equal(got, want) {
		t.Fatalf("routes %v, want %v", got, want)
	}

	// The same host and path in another app is a conflict that names the
	// app that has it, and leaves nothing behind.
	_, err := s.PutApp(ctx, withPath("clash", "www.example.com", "/api"))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "host www.example.com with path /api is routed by app api") {
		t.Fatalf("a path another app has: %v", err)
	}
	if _, err := s.GetApp(ctx, "clash"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused put left the app behind: %v", err)
	}
	// The whole host is still one app's, with the message it had before.
	_, err = s.PutApp(ctx, routed("clash", 1, "www.example.com"))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "host www.example.com is routed by app web;") {
		t.Fatalf("the whole host another app has: %v", err)
	}

	// An app that changes its path frees the old one, which another app
	// can then take; a deleted app frees its paths at once.
	mustPut(t, s, withPath("api", "www.example.com", "/v1"))
	mustPut(t, s, withPath("clash", "www.example.com", "/api"))
	if err := s.DeleteApp(ctx, "admin"); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, withPath("other", "www.example.com", "/admin"))
	want = []string{"www.example.com=web", "www.example.com/admin=other", "www.example.com/api=clash",
		"www.example.com/api/v2=v2", "www.example.com/v1=api"}
	if got := routeKeys(t, s); !slices.Equal(got, want) {
		t.Fatalf("routes %v, want %v", got, want)
	}
	// The whole host can go, and the paths stay.
	mustPut(t, s, routed("web", 1, "web.example.com"))
	if got := routeKeys(t, s); len(got) != 5 || got[0] != "web.example.com=web" || got[1] != "www.example.com/admin=other" {
		t.Fatalf("routes after the whole host left: %v", got)
	}
}

// Path routes get the endpoints of their own app's port.
func TestRouteTableWithPaths(t *testing.T) {
	e := newRolloutEnv(t)
	e.renewAt(node1, "10.0.0.1")
	e.put(routed("web", 1, "www.example.com"))
	api := withPath("api", "www.example.com", "/api")
	api.Routes = append(api.Routes, spec.Route{Host: "www.example.com", Path: "/api-admin", Port: 81})
	e.put(api)
	c := container("api", 0, 1)
	c.RoutePorts = []RoutePort{{Port: 80, HostPort: 40000}, {Port: 81, HostPort: 40001}}
	e.report(node1, routedContainer("web", 0, 1, 32768), c)
	rt, err := e.s.RouteTable(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rt {
		got[r.Key().String()] = r.App + " " + strings.Join(r.Endpoints, ",")
	}
	want := map[string]string{"www.example.com": "web 10.0.0.1:32768", "www.example.com/api": "api 10.0.0.1:40000",
		"www.example.com/api-admin": "api 10.0.0.1:40001"}
	if len(got) != len(want) {
		t.Fatalf("table %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("table %v, want %v", got, want)
		}
	}
	if rt[0].Path != "" || rt[1].Path != "/api" || rt[2].Path != "/api-admin" {
		t.Fatalf("order %+v", rt)
	}
}
