package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// routesAPI serves the API on a store with two nodes, one of them with an
// address, and the readers the routes and status warnings read.
func routesAPI(t *testing.T) (*store.Store, *nodeapi.RouteReaders) {
	t.Helper()
	home := isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, store.Node{ID: "node-s", Name: "srv", CPU: 4, Memory: 8 << 30, Address: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	readers := nodeapi.NewRouteReaders()
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RouteReaders: readers})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	return st, readers
}

func TestRoutesCommandAndPathRoutes(t *testing.T) {
	_, readers := routesAPI(t)
	out, _, err := run(t, "", "routes")
	if err != nil || strings.TrimSpace(out) != "No routes." {
		t.Fatalf("routes with none: %q, %v", out, err)
	}

	web := "name: web\nimage: nginx:alpine\nroutes:\n  - host: www.example.com\n    port: 80\n"
	api := "name: api\nimage: nginx:alpine\nroutes:\n  - host: www.example.com\n    path: /api/\n    port: 8080\n  - host: api.example.com\n    port: 8080\n"
	for _, f := range []string{web, api} {
		if _, _, err := run(t, f, "deploy", "-f", "-"); err != nil {
			t.Fatalf("deploy: %v\n%s", err, f)
		}
	}
	// The same host and path in another app is refused, naming the app.
	clash := "name: clash\nimage: nginx:alpine\nroutes:\n  - {host: WWW.example.com, path: /api, port: 80}\n"
	if _, _, err := run(t, clash, "deploy", "-f", "-"); err == nil || !strings.Contains(err.Error(), "host www.example.com with path /api is routed by app api") {
		t.Fatalf("a clash: %v", err)
	}
	// A path the spec does not allow is refused with the reason.
	bad := "name: bad\nimage: nginx:alpine\nroutes:\n  - {host: bad.example.com, path: /a/../b, port: 80}\n"
	if _, _, err := run(t, bad, "deploy", "-f", "-"); err == nil || !strings.Contains(err.Error(), "a .. segment") {
		t.Fatalf("a bad path: %v", err)
	}

	out, stderr, err := run(t, "", "routes")
	if err != nil {
		t.Fatal(err)
	}
	rows := tableRows(t, out)
	for _, want := range []map[string]string{
		{"HOST": "api.example.com", "PATH": "/", "APP": "api", "PORT": "8080", "ENDPOINTS": "-"},
		{"HOST": "www.example.com", "PATH": "/", "APP": "web", "PORT": "80", "ENDPOINTS": "-"},
		{"HOST": "www.example.com", "PATH": "/api", "APP": "api", "PORT": "8080", "ENDPOINTS": "-"},
	} {
		if !hasRow(rows, want) {
			t.Errorf("no row %v in:\n%s", want, out)
		}
	}
	if len(rows) != 3 || stderr != "" {
		t.Fatalf("routes:\n%s\nstderr: %s", out, stderr)
	}

	// A router of a build before path routes read the table: routes warns
	// on standard error, and so does the status of each app on its host.
	readers.Seen("node-w", "v2.2.0-old", false, []string{"www.example.com"}, time.Now())
	readers.Seen("node-x", "", false, nil, time.Now())
	out, stderr, err = run(t, "", "routes")
	if err != nil || len(tableRows(t, out)) != 3 {
		t.Fatalf("routes: %q, %v", out, err)
	}
	for _, want := range []string{
		"warning: the router on node node-w (v2.2.0-old) is of a build before path routes, and answers 503 for www.example.com; upgrade the node",
		"note: the router on node node-x (no version) is of a build before path routes",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("routes stderr lacks %q:\n%s", want, stderr)
		}
	}
	out, _, err = run(t, "", "routes", "--json")
	var list server.RouteList
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list.Routes) != 3 || len(list.OldRouters) != 2 ||
		list.Routes[2].Path != "/api" || list.Routes[1].Path != "/" || list.Routes[2].Endpoints == nil {
		t.Fatalf("routes --json: %s, %v", out, err)
	}

	out, _, err = run(t, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"web", "api"} {
		want := app + ": warning: the router on node node-w (v2.2.0-old) is of a build before path routes, and answers 503 for www.example.com, which has path routes; upgrade the node"
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "node-x") {
		t.Errorf("status warns about a router that holds back no host:\n%s", out)
	}
	out, _, err = run(t, "", "status", "web", "--json")
	var one server.AppStatus
	if err != nil || json.Unmarshal([]byte(out), &one) != nil || len(one.Warnings) != 1 {
		t.Fatalf("status web --json: %s, %v", out, err)
	}

	// The api app's route of a whole host of its own is not held back by
	// the old router; once that router reads paths, nothing is.
	readers.Seen("node-w", "v2.2.1", true, nil, time.Now())
	readers.Seen("node-x", "v2.2.1", true, nil, time.Now())
	out, _, err = run(t, "", "status")
	if err != nil || strings.Contains(out, "warning") {
		t.Fatalf("status after the upgrade: %s, %v", out, err)
	}
	if _, stderr, _ = run(t, "", "routes"); stderr != "" {
		t.Fatalf("routes after the upgrade: %s", stderr)
	}
}

// A spec with paths round-trips through the API as the server stored it.
func TestDeployWithAPathShowsIt(t *testing.T) {
	st, _ := routesAPI(t)
	f := "name: api\nimage: nginx:alpine\nroutes:\n  - {host: www.example.com, path: /v1/, port: 8080}\n  - {host: www.example.com, path: /, port: 8081}\n"
	if _, _, err := run(t, f, "deploy", "-f", "-"); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetApp(context.Background(), "api")
	if err != nil {
		t.Fatal(err)
	}
	want := []spec.Route{{Host: "www.example.com", Path: "/v1", Port: 8080}, {Host: "www.example.com", Port: 8081}}
	if len(a.Spec.Routes) != 2 || a.Spec.Routes[0] != want[0] || a.Spec.Routes[1] != want[1] {
		t.Fatalf("stored routes %+v", a.Spec.Routes)
	}
	// The same spec again is unchanged: "/v1/" and "/" are stored as "/v1"
	// and the whole host.
	out, _, err := run(t, f, "deploy", "-f", "-")
	if err != nil || !strings.Contains(out, "unchanged") {
		t.Fatalf("deploy again: %q, %v", out, err)
	}
}
