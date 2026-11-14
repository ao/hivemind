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
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// TestNodesCommands drives hivemind nodes and hivemind node remove against
// the real API handler.
func TestNodesCommands(t *testing.T) {
	home := isolate(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(home, "d", storeFileName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: [32]byte{1}, NodeID: "node-s"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Node{{ID: "node-s", Name: "srv", CPU: 4, Memory: 8 << 30}, {ID: "node-w", Name: "wrk", CPU: 2, Memory: 1 << 30}} {
		if err := st.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	// One replica per node: the scheduler spreads an app's replicas.
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.NewToken()
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.HashToken(tok), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: "node-s", Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)

	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[0], "LEASE AGE") {
		t.Fatalf("nodes table:\n%s", out)
	}
	for _, want := range [][]string{{"node-s", "srv", "server", "4", "8.0Gi", "ready", "1"}, {"node-w", "wrk", "worker", "2", "1.0Gi", "ready", "1"}} {
		found := false
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) == 8 && f[0] == want[0] && f[1] == want[1] && f[2] == want[2] && f[3] == want[3] && f[4] == want[4] && f[6] == want[5] && f[7] == want[6] {
				found = true
			}
		}
		if !found {
			t.Errorf("no row %v in:\n%s", want, out)
		}
	}
	out, _, err = run(t, "", "nodes", "--json")
	var views []server.NodeView
	if err != nil || json.Unmarshal([]byte(out), &views) != nil || len(views) != 2 {
		t.Fatalf("nodes --json: %q, %v", out, err)
	}

	// The worker still renews its lease, so it needs --force.
	if _, _, err := run(t, "", "node", "remove", "node-w"); err == nil || !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("remove a live node: %v", err)
	}
	if _, _, err := run(t, "", "node", "remove", "node-s", "--force"); err == nil || !strings.Contains(err.Error(), "runs the server") {
		t.Fatalf("remove the server's node: %v", err)
	}
	if _, _, err := run(t, "", "node", "remove"); err == nil {
		t.Fatal("node remove with no ID succeeded")
	}
	out, _, err = run(t, "", "node", "remove", "node-w", "--force")
	if err != nil || out != "node-w (wrk): removed; 1 replica placed again\n" {
		t.Fatalf("node remove --force: %q, %v", out, err)
	}
	if _, _, err := run(t, "", "node", "remove", "node-w", "--force"); err == nil || !strings.Contains(err.Error(), "already removed") {
		t.Fatalf("second remove: %v", err)
	}
	out, _, err = run(t, "", "nodes")
	if err != nil || strings.Contains(out, "node-w") || !strings.Contains(out, "node-s") {
		t.Fatalf("nodes after removal: %q, %v", out, err)
	}
	// Both replicas of web are now on the server's node.
	reps, _ := st.Replicas(ctx, "web")
	for _, r := range reps {
		if r.NodeID != "node-s" {
			t.Fatalf("replica %d on %q after the removal", r.Index, r.NodeID)
		}
	}
}

func TestBytesIEC(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1.0Ki", 1536: "1.5Ki", 8 << 30: "8.0Gi", 3 << 40: "3.0Ti"} {
		if got := bytesIEC(n); got != want {
			t.Errorf("bytesIEC(%d) = %q, want %q", n, got, want)
		}
	}
}
