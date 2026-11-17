package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// oldRouterTable is the routing table as a router of a build before path
// routes decodes it: strictly, with host and endpoints only.
type oldRouterTable struct {
	Routes []struct {
		Host      string   `json:"host"`
		Endpoints []string `json:"endpoints"`
	} `json:"routes"`
}

// A router that asks for paths gets every route; one that does not, as a
// router of a build before path routes, gets a table it can decode, with no
// endpoints for the hosts that have path routes. The server logs that once
// per change, and RouteReaders names the old router until it reads paths.
func TestRoutesForRoutersWithAndWithoutPaths(t *testing.T) {
	logs := &lockedBuffer{}
	readers := nodeapi.NewRouteReaders()
	fx := newFixtureWith(t, func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(logs, nil))
		c.RouteReaders = readers
	}, store.WithMinReady(0))
	ctx := context.Background()
	id, cert := fx.enroll(fx.issue(time.Hour))
	c := fx.apiClient(cert)
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w", CPU: 4, Memory: 4 << 30, Address: "10.9.0.5"}); err != nil {
		t.Fatal(err)
	}
	put := func(a spec.App) {
		t.Helper()
		a.Image, a.Replicas = "nginx", 1
		a.Normalize()
		if _, err := fx.st.PutApp(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	put(spec.App{Name: "web", Routes: []spec.Route{{Host: "www.example.com", Port: 80}, {Host: "only.example.com", Port: 80}}})
	put(spec.App{Name: "api", Routes: []spec.Route{{Host: "www.example.com", Path: "/api", Port: 8080}}})
	if err := c.ReportStatus(ctx, id, []store.Status{
		{Name: "hivemind-web-0-g1", App: "web", Replica: 0, Generation: 1, State: "running", Ready: true,
			RoutePorts: []store.RoutePort{{Port: 80, HostPort: 32768}}},
		{Name: "hivemind-api-0-g1", App: "api", Replica: 0, Generation: 1, State: "running", Ready: true,
			RoutePorts: []store.RoutePort{{Port: 8080, HostPort: 32769}}},
	}); err != nil {
		t.Fatal(err)
	}

	// This build's router.
	routes, err := c.Routes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, r := range routes {
		got[r.Host+r.Path] = r.Endpoints
	}
	if len(got) != 3 || !slices.Equal(got["www.example.com"], []string{"10.9.0.5:32768"}) ||
		!slices.Equal(got["www.example.com/api"], []string{"10.9.0.5:32769"}) || !slices.Equal(got["only.example.com"], []string{"10.9.0.5:32768"}) {
		t.Fatalf("routes with paths: %v", got)
	}
	if old := readers.Old(time.Now()); len(old) != 0 {
		t.Fatalf("a router that reads paths is old: %+v", old)
	}

	// A router before paths: no parameter, a version, a strict decoder.
	oldRead := func() oldRouterTable {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, fx.nodeURL(nodeapi.RoutesPath(id)), nil)
		req.Header.Set(nodeapi.VersionHeader, "v2.2.0-old")
		resp, err := fx.nodeClient(cert).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET routes: %d %s", resp.StatusCode, b)
		}
		var tab oldRouterTable
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&tab); err != nil {
			t.Fatalf("a router before paths cannot decode its table: %v\n%s", err, b)
		}
		return tab
	}
	for range 3 {
		tab := oldRead()
		hosts := map[string][]string{}
		for _, r := range tab.Routes {
			if _, twice := hosts[r.Host]; twice {
				t.Fatalf("host %s twice: %+v", r.Host, tab)
			}
			hosts[r.Host] = r.Endpoints
		}
		if len(hosts) != 2 || hosts["www.example.com"] == nil || len(hosts["www.example.com"]) != 0 ||
			!slices.Equal(hosts["only.example.com"], []string{"10.9.0.5:32768"}) {
			t.Fatalf("the table of a router before paths: %+v", hosts)
		}
	}
	old := readers.Old(time.Now())
	if len(old) != 1 || old[0].Node != id || old[0].Version != "v2.2.0-old" || !slices.Equal(old[0].Hosts, []string{"www.example.com"}) {
		t.Fatalf("old routers %+v", old)
	}
	if n := strings.Count(logs.String(), "is of a build before path routes"); n != 1 {
		t.Fatalf("the warning was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "hosts=[www.example.com]") || !strings.Contains(logs.String(), "version=v2.2.0-old") {
		t.Fatalf("the warning does not name the hosts and version:\n%s", logs.String())
	}

	// The node is upgraded: its router reads paths, and is no longer old.
	if _, err := c.Routes(ctx, id); err != nil {
		t.Fatal(err)
	}
	if old := readers.Old(time.Now()); len(old) != 0 {
		t.Fatalf("old routers after the upgrade: %+v", old)
	}
}
