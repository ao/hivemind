package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// startedEnv is a store whose clock the test sets, and a server that started
// at a given time, both against the real clock that the node view reads.
type startedEnv struct {
	t     *testing.T
	st    *store.Store
	tok   string
	mu    sync.Mutex
	clock time.Time
}

func (e *startedEnv) now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.clock
}

func (e *startedEnv) set(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = t
}

func newStartedEnv(t *testing.T) *startedEnv {
	t.Helper()
	e := &startedEnv{t: t, clock: time.Now().Add(-time.Minute)}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"),
		store.WithClock(e.now), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	e.tok = tok
	return e
}

func (e *startedEnv) get(started time.Time, path string, out any) {
	e.t.Helper()
	sessions, err := auth.NewSessions(time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := auth.NewAuthenticator(auth.HashToken(e.tok), sessions)
	if err != nil {
		e.t.Fatal(err)
	}
	srv, err := New(Config{Store: e.st, Runtime: &fakeRuntime{}, NodeID: "node-a", Auth: a, Started: started,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+e.tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		e.t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		e.t.Fatal(err)
	}
}

// A report made to an earlier server process, as the store holds after a
// restart or a restore, is not shown as the replica's state: after a restore
// on a new machine its container is gone. The replica is "unknown", and does
// not count as running, until its node reports to this process.
func TestReportsFromBeforeTheStartAreUnknown(t *testing.T) {
	e := newStartedEnv(t)
	ctx := context.Background()
	if err := e.st.UpsertNode(ctx, store.Node{ID: "node-a", Name: "a", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	report := func() {
		if err := e.st.ReportStatus(ctx, "node-a", []store.Status{{Name: "hivemind-web-0-g1", ContainerID: "c0",
			App: "web", Replica: 0, Generation: 1, State: "running", Ready: true}}); err != nil {
			t.Fatal(err)
		}
	}
	report() // a minute ago, by the store's clock

	var got AppStatus
	e.get(time.Time{}, "/v1/apps/web/status", &got)
	if got.Replicas[0].State != "running" || got.Running != 1 {
		t.Fatalf("with no start time: %+v, running %d; want the report as it is", got.Replicas[0], got.Running)
	}

	started := time.Now().Add(-5 * time.Second)
	e.get(started, "/v1/apps/web/status", &got)
	r := got.Replicas[0]
	if r.State != "unknown" || r.Ready || got.Running != 0 || r.ContainerID != "c0" {
		t.Fatalf("report from before the start: %+v, running %d; want unknown, not ready, not running, the last container ID", r, got.Running)
	}

	e.set(time.Now())
	report()
	e.get(started, "/v1/apps/web/status", &got)
	if got.Replicas[0].State != "running" || got.Running != 1 {
		t.Fatalf("after a report to this process: %+v, running %d", got.Replicas[0], got.Running)
	}
}

// In the start grace after serve starts, a node that was up before the start
// and has not reported to this process is "waiting", as the sweep does not
// count it lost. When the grace ends with no report from any node that was
// waiting, they all stay waiting (the store's hold); once one reports, it
// is ready at once, and the others are lost. A node whose lease had lapsed
// before the server went down is lost at once.
func TestNodesWaitForTheirFirstReportAfterTheStart(t *testing.T) {
	e := newStartedEnv(t)
	ctx := context.Background()
	// The server went down two hours ago. node-b had stopped renewing 20 s
	// before that, so it was lost already.
	down := time.Now().Add(-2 * time.Hour)
	e.set(down.Add(-20 * time.Second))
	if err := e.st.UpsertNode(ctx, store.Node{ID: "node-b", Name: "b", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	e.set(down)
	for _, id := range []string{"node-a", "node-c"} {
		if err := e.st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	view := func(started time.Time) map[string]string {
		t.Helper()
		var nodes []NodeView
		e.get(started, "/v1/nodes", &nodes)
		out := map[string]string{}
		for _, n := range nodes {
			out[n.ID] = n.State
		}
		return out
	}
	states := func(started time.Time) map[string]string {
		t.Helper()
		if !started.IsZero() {
			if err := e.st.SetStarted(ctx, started, 2*time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		return view(started)
	}
	if s := states(time.Time{}); s["node-a"] != "lost" || s["node-b"] != "lost" || s["node-c"] != "lost" {
		t.Fatalf("leases two hours old with no start: %v, want all lost", s)
	}
	for _, after := range []time.Duration{5 * time.Second, 20 * time.Second, 115 * time.Second, 125 * time.Second, time.Hour} {
		if s := states(time.Now().Add(-after)); s["node-a"] != "waiting" || s["node-c"] != "waiting" || s["node-b"] != "lost" {
			t.Fatalf("%s after the start, no report since: %v; want node-a and node-c waiting, node-b lost", after, s)
		}
	}
	started := time.Now().Add(-125 * time.Second)
	states(started)
	e.set(time.Now())
	if err := e.st.RenewLease(ctx, "node-c"); err != nil {
		t.Fatal(err)
	}
	if s := view(started); s["node-a"] != "lost" || s["node-c"] != "ready" {
		t.Fatalf("125 s after a start with a 2-minute grace, once node-c reported: %v, want node-a lost, node-c ready", s)
	}
	if err := e.st.RenewLease(ctx, "node-a"); err != nil {
		t.Fatal(err)
	}
	if s := view(started); s["node-a"] != "ready" {
		t.Fatalf("after a report to this process: %v, want node-a ready", s)
	}
}

// raw is get for a reply that is not JSON, such as /metrics.
func (e *startedEnv) raw(path string) string {
	e.t.Helper()
	sessions, err := auth.NewSessions(time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := auth.NewAuthenticator(auth.HashToken(e.tok), sessions)
	if err != nil {
		e.t.Fatal(err)
	}
	srv, err := New(Config{Store: e.st, Runtime: &fakeRuntime{}, NodeID: "node-a", Auth: a,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+e.tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		e.t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// A volume replica keeps its node when the node is lost, and the node's last
// report stays in the store. That report must not make the replica look
// running and ready, in status or in metrics, while its node is down: the
// scale run on ten machines saw 500 of 500 running with three nodes off.
func TestALostNodesReportIsNotCurrent(t *testing.T) {
	e := newStartedEnv(t)
	ctx := context.Background()
	for _, id := range []string{"node-a", "node-b"} {
		if err := e.st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.PutApp(ctx, spec.App{Name: "db", Image: "redis:7-alpine", Replicas: 1,
		Volumes: []spec.Volume{{Name: "data", Path: "/data"}}}); err != nil {
		t.Fatal(err)
	}
	reps, err := e.st.Replicas(ctx, "db")
	if err != nil || len(reps) != 1 || reps[0].NodeID == "" {
		t.Fatalf("db not placed: %+v, %v", reps, err)
	}
	on, other := reps[0].NodeID, "node-a"
	if on == "node-a" {
		other = "node-b"
	}
	report := func() {
		if err := e.st.ReportStatus(ctx, on, []store.Status{{Name: "hivemind-db-0-g1", ContainerID: "c0",
			App: "db", Replica: 0, Generation: 1, State: "running", Ready: true}}); err != nil {
			t.Fatal(err)
		}
	}
	report()
	var got AppStatus
	e.get(time.Time{}, "/v1/apps/db/status", &got)
	if got.Running != 1 || got.Ready != 1 || got.Replicas[0].State != "running" {
		t.Fatalf("with its node up: %+v", got)
	}

	// The node goes quiet; the other renews. The sweep keeps the volume
	// replica on its node, with a reason.
	e.set(e.now().Add(20 * time.Second))
	if err := e.st.RenewLease(ctx, other); err != nil {
		t.Fatal(err)
	}
	if lost, err := e.st.ReconcileNodes(ctx); err != nil || len(lost) != 1 || lost[0] != on {
		t.Fatalf("sweep: lost %v, %v; want [%s]", lost, err, on)
	}
	e.get(time.Time{}, "/v1/apps/db/status", &got)
	r := got.Replicas[0]
	if got.Running != 0 || got.Ready != 0 || r.State != "unknown" || r.Ready || r.NodeID != on || r.PendingReason == "" {
		t.Fatalf("with its node lost: running %d, ready %d, replica %+v; want unknown on %s with a reason, nothing counted",
			got.Running, got.Ready, r, on)
	}
	m := e.raw("/metrics")
	for _, line := range []string{`hivemind_replicas_running{app="db"} 0`, `hivemind_replicas_ready{app="db"} 0`} {
		if !strings.Contains(m, line) {
			t.Errorf("metrics lack %q while the node is lost", line)
		}
	}

	// The node comes back and reports: running again.
	report()
	e.get(time.Time{}, "/v1/apps/db/status", &got)
	if got.Running != 1 || got.Replicas[0].State != "running" {
		t.Fatalf("after the node reports again: %+v", got)
	}
	if m := e.raw("/metrics"); !strings.Contains(m, `hivemind_replicas_running{app="db"} 1`) {
		t.Error("metrics do not count the replica again after its node reports")
	}
}
