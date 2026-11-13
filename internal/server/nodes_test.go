package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/store"
)

func nodesOf(t *testing.T, h *harness) []NodeView {
	t.Helper()
	rec := h.do(t, "GET", "/v1/nodes", "")
	wantStatus(t, rec, http.StatusOK)
	var out []NodeView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestNodesListAndRemove(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.UpsertNode(ctx, store.Node{ID: "node-2", Name: "two", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", `{"name":"web","image":"nginx:alpine","replicas":2}`), http.StatusCreated)

	nodes := nodesOf(t, h)
	if len(nodes) != 2 || nodes[0].ID != testNode || nodes[1].ID != "node-2" {
		t.Fatalf("nodes: %+v", nodes)
	}
	total := 0
	for _, n := range nodes {
		if n.State != "ready" || n.LeaseAgeSeconds < 0 || n.LeaseAgeSeconds > 15 || n.CPU != 2 || n.Memory != 1<<30 {
			t.Errorf("node %+v", n)
		}
		total += n.Replicas
	}
	if total != 2 || !nodes[0].Server || nodes[1].Server || nodes[1].Name != "two" {
		t.Fatalf("nodes: %+v", nodes)
	}
	onTwo := nodes[1].Replicas

	// The server's own node, a malformed ID, a bad force value, an unknown
	// node, and a node still renewing its lease are all refused.
	rec := h.do(t, "DELETE", "/v1/nodes/"+testNode+"?force=true", "")
	wantStatus(t, rec, http.StatusConflict)
	if !strings.Contains(errorOf(t, rec), "runs the server") {
		t.Fatalf("removing the server's node: %s", rec.Body.String())
	}
	wantStatus(t, h.do(t, "DELETE", "/v1/nodes/Node_2", ""), http.StatusBadRequest)
	wantStatus(t, h.do(t, "DELETE", "/v1/nodes/node-2?force=yes", ""), http.StatusBadRequest)
	wantStatus(t, h.do(t, "DELETE", "/v1/nodes/node-9?force=true", ""), http.StatusNotFound)
	rec = h.do(t, "DELETE", "/v1/nodes/node-2", "")
	wantStatus(t, rec, http.StatusConflict)
	if !strings.Contains(errorOf(t, rec), "still running") {
		t.Fatalf("removing a live node: %s", rec.Body.String())
	}

	before := h.trigger
	rec = h.do(t, "DELETE", "/v1/nodes/node-2?force=true", "")
	wantStatus(t, rec, http.StatusOK)
	var reply RemoveNodeReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != "node-2" || reply.Name != "two" || reply.Rescheduled != onTwo || reply.Held != 0 {
		t.Fatalf("reply %+v, want %d rescheduled", reply, onTwo)
	}
	if h.trigger == before {
		t.Fatal("a removal did not trigger a reconcile")
	}
	if nodes := nodesOf(t, h); len(nodes) != 1 || nodes[0].ID != testNode || nodes[0].Replicas != 2 {
		t.Fatalf("nodes after removal: %+v", nodes)
	}
	rec = h.do(t, "DELETE", "/v1/nodes/node-2?force=true", "")
	wantStatus(t, rec, http.StatusNotFound)
	if !strings.Contains(errorOf(t, rec), "already removed") {
		t.Fatalf("second removal: %s", rec.Body.String())
	}
	if !strings.Contains(h.logs.String(), "node removed") {
		t.Fatal("the removal is not logged")
	}
}
