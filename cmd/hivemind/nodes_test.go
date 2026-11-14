package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	for _, want := range [][]string{{"node-s", "srv", "server", "4", "8.0Gi", "ready", "eligible", "1"}, {"node-w", "wrk", "worker", "2", "1.0Gi", "ready", "eligible", "1"}} {
		found := false
		for _, l := range lines[1:] {
			f := strings.Fields(l)
			if len(f) == 10 && f[9] == "-" && f[0] == want[0] && f[1] == want[1] && f[2] == want[2] && f[3] == want[3] && f[4] == want[4] &&
				f[6] == want[5] && f[7] == want[6] && f[8] == want[7] {
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

func TestCertExpiry(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	for _, c := range []struct {
		t    *time.Time
		want string
	}{
		{nil, "-"},
		{at(365 * 24 * time.Hour), "2027-11-14 (in 365d)"},
		{at(90 * time.Minute), "2026-11-14 (in 1h30m)"},
		{at(9*time.Minute + 50*time.Second), "2026-11-14 (in 10m)"},
		{at(30 * time.Second), "2026-11-14 (in 30s)"},
		{at(-time.Hour), "2026-11-14 (expired)"},
	} {
		if got := certExpiry(c.t, now); got != c.want {
			t.Errorf("certExpiry = %q, want %q", got, c.want)
		}
	}
}

// nodesAPI starts the real API handler on a store with the server's node
// node-s and a worker node-w, and points the client commands at it. web has
// one replica on each node.
func nodesAPI(t *testing.T) *store.Store {
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
	for _, n := range []store.Node{{ID: "node-s", Name: "srv", CPU: 4, Memory: 8 << 30}, {ID: "node-w", Name: "wrk", CPU: 2, Memory: 1 << 30}} {
		if err := st.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
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
	t.Cleanup(ts.Close)
	t.Setenv(envServer, ts.URL)
	t.Setenv(envToken, tok)
	return st
}

// eligibilityOf returns the ELIGIBILITY column of each row of hivemind nodes.
func eligibilityOf(t *testing.T) map[string]string {
	t.Helper()
	out, _, err := run(t, "", "nodes")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n")[1:] {
		if f := strings.Fields(l); len(f) == 10 {
			got[f[0]] = f[7]
		}
	}
	return got
}

// TestNodeEligibilityAndDrainCommands drives hivemind node eligibility and
// hivemind node drain against the real API handler. The test plays the
// agents' reports and the server's drain steps.
func TestNodeEligibilityAndDrainCommands(t *testing.T) {
	st := nodesAPI(t)
	ctx := context.Background()
	old := drainPoll
	drainPoll = 10 * time.Millisecond
	t.Cleanup(func() { drainPoll = old })
	running := func(app string, idx int) store.Status {
		return store.Status{Name: fmt.Sprintf("hivemind-%s-%d-g1", app, idx), App: app, Replica: idx, Generation: 1, State: "running", Ready: true}
	}
	report := func(node string, sts ...store.Status) {
		t.Helper()
		if err := st.ReportStatus(ctx, node, sts); err != nil {
			t.Fatal(err)
		}
	}
	step := func() {
		t.Helper()
		if _, err := st.AdvanceDrains(ctx, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"node", "eligibility", "node-w"}, {"node", "eligibility", "node-w", "--enable", "--disable"}} {
		if _, _, err := run(t, "", args...); err == nil || !strings.Contains(err.Error(), "one of --enable and --disable") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, _, err := run(t, "", "node", "eligibility", "node-x", "--disable"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown node: %v", err)
	}
	out, _, err := run(t, "", "node", "eligibility", "node-w", "--disable")
	if err != nil || out != "node-w (wrk): ineligible; it keeps its replicas and takes no new ones\n" {
		t.Fatalf("disable: %q, %v", out, err)
	}
	if got := eligibilityOf(t); got["node-w"] != "ineligible" || got["node-s"] != "eligible" {
		t.Fatalf("nodes: %v", got)
	}

	for _, d := range []string{"0s", "-1m", "200h"} {
		if _, _, err := run(t, "", "node", "drain", "node-w", "--deadline="+d); err == nil || !strings.Contains(err.Error(), "--deadline must be") {
			t.Fatalf("--deadline %s: %v", d, err)
		}
	}
	// --no-wait starts the drain and prints its first step. web/1 is on
	// node-w, and its copy starts on node-s.
	out, _, err = run(t, "", "node", "drain", "node-w", "--no-wait", "--deadline", "5m")
	if err != nil || !strings.HasPrefix(out, "node-w: draining 1 replica; deadline ") || !strings.Contains(out, "\n  moving web/1 to node-s\n") ||
		!strings.Contains(out, "Not waiting.") {
		t.Fatalf("drain --no-wait: %q, %v", out, err)
	}
	if got := eligibilityOf(t); got["node-w"] != "draining" {
		t.Fatalf("nodes during the drain: %v", got)
	}
	out, _, err = run(t, "", "node", "drain", "node-w", "--no-wait", "--json")
	var v server.DrainView
	if err != nil || json.Unmarshal([]byte(out), &v) != nil || v.State != "draining" || v.Deadline.Sub(v.StartedAt) != 5*time.Minute {
		t.Fatalf("drain --no-wait --json on a draining node, which keeps its deadline: %q, %v", out, err)
	}

	// waitFor polls the store until ok is true.
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Without --no-wait the command waits for the drain to finish. Its new
	// deadline shows that its request has arrived.
	type result struct {
		out string
		err error
	}
	waited := make(chan result, 1)
	go func() {
		out, _, err := run(t, "", "node", "drain", "node-w", "--deadline", "6m")
		waited <- result{out, err}
	}()
	waitFor("the new deadline", func() bool {
		d, err := st.GetDrain(ctx, "node-w")
		return err == nil && d.Deadline.Sub(d.StartedAt) > 5*time.Minute
	})
	report("node-s", running("web", 0), running("web", 1))
	step()
	report("node-w")
	step()
	var r result
	select {
	case r = <-waited:
	case <-time.After(20 * time.Second):
		t.Fatal("node drain did not return after the drain finished")
	}
	if r.err != nil || !strings.HasSuffix(r.out, "node-w: drained; 1 moved, 0 stopped at the deadline and placed again, 0 left on the node\n") {
		t.Fatalf("waiting drain: %q, %v", r.out, r.err)
	}
	if got := eligibilityOf(t); got["node-w"] != "drained" {
		t.Fatalf("nodes after the drain: %v", got)
	}

	// Nothing runs on the drained node, so it is removed without --force.
	out, _, err = run(t, "", "node", "remove", "node-w")
	if err != nil || out != "node-w (wrk): removed; 0 replicas placed again\n" {
		t.Fatalf("remove the drained node: %q, %v", out, err)
	}

	// The server's own node can be drained. With no other node, its replicas
	// stay, and the waiting command ends when the drain is cancelled.
	go func() {
		out, _, err := run(t, "", "node", "drain", "node-s")
		waited <- result{out, err}
	}()
	waitFor("the drain of node-s", func() bool { _, err := st.GetDrain(ctx, "node-s"); return err == nil })
	out, _, err = run(t, "", "node", "eligibility", "node-s", "--enable")
	if err != nil || out != "node-s (srv): eligible; its drain was cancelled, and the replicas still on it stay\n" {
		t.Fatalf("enable: %q, %v", out, err)
	}
	select {
	case r = <-waited:
	case <-time.After(20 * time.Second):
		t.Fatal("node drain did not return after the drain was cancelled")
	}
	if r.err == nil || !strings.Contains(r.err.Error(), "ended before it finished") ||
		!strings.Contains(r.out, "  web/0 stays: no other node is ready and eligible\n") {
		t.Fatalf("cancelled drain: %q, %v", r.out, r.err)
	}
}

func TestBytesIEC(t *testing.T) {
	for n, want := range map[int64]string{0: "0B", 1023: "1023B", 1024: "1.0Ki", 1536: "1.5Ki", 8 << 30: "8.0Gi", 3 << 40: "3.0Ti"} {
		if got := bytesIEC(n); got != want {
			t.Errorf("bytesIEC(%d) = %q, want %q", n, got, want)
		}
	}
}
