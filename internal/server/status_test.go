package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// A lost node keeps its last report in the store. When its replica has moved,
// or is pending, the status must not describe that replica with the lost
// node's container. It must say pending.
func TestLostNodesLastReportDoesNotDescribeAReplica(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	placeOK := true
	place := func(nodes []store.Node, _ []store.Assignment, _ spec.App, _ int, _ time.Time) (string, error) {
		if !placeOK {
			return "", errors.New("no node has room")
		}
		for _, n := range nodes {
			if n.ID == "node-a" {
				return n.ID, nil
			}
		}
		return "", errors.New("node-a is not registered")
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hivemind.db"), store.WithClock(clock),
		store.WithPlacement(place), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"node-a", "node-b"} {
		if err := st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReportStatus(ctx, "node-a", []store.Status{{Name: "hivemind-web-0-g1", ContainerID: "c0",
		App: "web", Replica: 0, Generation: 1, State: "running", Ready: true}}); err != nil {
		t.Fatal(err)
	}

	// node-a stops renewing. node-b does, and the sweep releases node-a. The
	// placement hook has no room now, so the replica stays pending.
	now = now.Add(20 * time.Second)
	if err := st.UpsertNode(ctx, store.Node{ID: "node-b", Name: "node-b", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	placeOK = false
	if lost, err := st.ReconcileNodes(ctx); err != nil || len(lost) != 1 || lost[0] != "node-a" {
		t.Fatalf("sweep: lost %v, err %v", lost, err)
	}
	if r := replicasFor(t, st, "web")[0]; r.NodeID != "" {
		t.Fatalf("replica still on %q after its node was lost: %+v", r.NodeID, r)
	}

	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := auth.NewSessions(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.NewAuthenticator(auth.HashToken(tok), sessions)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{Store: st, Runtime: &fakeRuntime{}, NodeID: "node-b", Auth: a,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/apps/web/status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var got AppStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Replicas) != 1 {
		t.Fatalf("replicas %+v", got.Replicas)
	}
	r := got.Replicas[0]
	if r.State != "pending" || r.ContainerID != "" || got.Running != 0 {
		t.Fatalf("pending replica reports the lost node's container: state %q, container %q, running %d",
			r.State, r.ContainerID, got.Running)
	}
	if r.PendingReason == "" {
		t.Fatal("pending replica has no reason")
	}
}

func replicasFor(t *testing.T, st *store.Store, app string) []store.Replica {
	t.Helper()
	rs, err := st.Replicas(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}
