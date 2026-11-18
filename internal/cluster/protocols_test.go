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

// A router of this build gets each route's protocol. One of a build with
// paths and before protocols asks for paths only, decodes strictly, and
// gets no protocol field and no endpoints for the h2c routes, so it answers
// 503 for them rather than speak HTTP/1.1 to an h2c replica. The server logs
// that once per change, and RouteReaders names it with the routes.
func TestRoutesForRoutersWithAndWithoutProtocols(t *testing.T) {
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
	a := spec.App{Name: "grpc", Image: "example/grpc", Replicas: 1, Routes: []spec.Route{
		{Host: "grpc.example.com", Port: 50051, Protocol: "h2c"},
		{Host: "www.example.com", Port: 8080},
		{Host: "www.example.com", Path: "/echo.Echo", Port: 50051, Protocol: "h2c"},
	}}
	a.Normalize()
	if _, err := fx.st.PutApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.ReportStatus(ctx, id, []store.Status{{Name: "hivemind-grpc-0-g1", App: "grpc", Replica: 0, Generation: 1,
		State: "running", Ready: true, RoutePorts: []store.RoutePort{{Port: 50051, HostPort: 32768}, {Port: 8080, HostPort: 32769}}}}); err != nil {
		t.Fatal(err)
	}

	routes, err := c.Routes(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range routes {
		got[r.Host+r.Path] = r.Protocol + " " + strings.Join(r.Endpoints, ",")
	}
	want := map[string]string{"grpc.example.com": "h2c 10.9.0.5:32768", "www.example.com": " 10.9.0.5:32769",
		"www.example.com/echo.Echo": "h2c 10.9.0.5:32768"}
	if len(got) != len(want) {
		t.Fatalf("routes %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("route %s: %q, want %q", k, got[k], v)
		}
	}
	if old := readers.Old(time.Now()); len(old) != 0 {
		t.Fatalf("a router that reads protocols is old: %+v", old)
	}

	pathsRead := func() map[string][]string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, fx.nodeURL(nodeapi.RoutesPath(id))+"?paths=1", nil)
		req.Header.Set(nodeapi.VersionHeader, "v2.2.0-paths")
		resp, err := fx.nodeClient(cert).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var tab struct {
			Routes []struct {
				Host      string   `json:"host"`
				Path      string   `json:"path"`
				Endpoints []string `json:"endpoints"`
			} `json:"routes"`
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if resp.StatusCode != http.StatusOK || dec.Decode(&tab) != nil {
			t.Fatalf("a router before protocols cannot decode its table: %d %s", resp.StatusCode, b)
		}
		out := map[string][]string{}
		for _, r := range tab.Routes {
			out[r.Host+r.Path] = r.Endpoints
		}
		return out
	}
	for range 3 {
		tab := pathsRead()
		if len(tab) != 3 || tab["grpc.example.com"] == nil || len(tab["grpc.example.com"]) != 0 ||
			len(tab["www.example.com/echo.Echo"]) != 0 || !slices.Equal(tab["www.example.com"], []string{"10.9.0.5:32769"}) {
			t.Fatalf("the table of a router before protocols: %v", tab)
		}
	}
	old := readers.Old(time.Now())
	if len(old) != 1 || old[0].Node != id || !old[0].Paths || old[0].Protocols || len(old[0].Hosts) != 0 ||
		!slices.Equal(old[0].H2C, []string{"grpc.example.com", "www.example.com/echo.Echo"}) {
		t.Fatalf("old routers %+v", old)
	}
	if n := strings.Count(logs.String(), "is of a build before route protocols"); n != 1 {
		t.Fatalf("the warning was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "routes=\"[grpc.example.com www.example.com/echo.Echo]\"") || !strings.Contains(logs.String(), "version=v2.2.0-paths") {
		t.Fatalf("the warning does not name the routes and version:\n%s", logs.String())
	}

	if _, err := c.Routes(ctx, id); err != nil {
		t.Fatal(err)
	}
	if old := readers.Old(time.Now()); len(old) != 0 {
		t.Fatalf("old routers after the upgrade: %+v", old)
	}
}
