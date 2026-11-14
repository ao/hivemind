package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func drainOf(t *testing.T, rec *httptest.ResponseRecorder) DrainView {
	t.Helper()
	var d DrainView
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNodeEligibilityAndDrain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.UpsertNode(ctx, store.Node{ID: "node-2", Name: "two", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	// One replica on each node: the scheduler spreads them.
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", `{"name":"web","image":"nginx:alpine","replicas":2}`), http.StatusCreated)
	for _, n := range nodesOf(t, h) {
		if !n.Eligible || n.Drain != nil || n.Replicas != 1 {
			t.Fatalf("node before: %+v", n)
		}
	}

	// Bad requests change nothing.
	for _, c := range []struct{ path, body string }{
		{"/v1/nodes/node-2/eligibility", `{}`},
		{"/v1/nodes/node-2/eligibility", `{"eligible":"no"}`},
		{"/v1/nodes/node-2/eligibility", `{"eligible":false,"x":1}`},
		{"/v1/nodes/Node_2/eligibility", `{"eligible":false}`},
		{"/v1/nodes/node-2/drain", `{"deadline":"soon"}`},
		{"/v1/nodes/node-2/drain", `{"deadline":"0s"}`},
		{"/v1/nodes/node-2/drain", `{"deadline":"200h"}`},
		{"/v1/nodes/Node_2/drain", `{}`},
	} {
		wantStatus(t, h.do(t, "POST", c.path, c.body), http.StatusBadRequest)
	}
	wantStatus(t, h.do(t, "POST", "/v1/nodes/node-9/eligibility", `{"eligible":false}`), http.StatusNotFound)
	wantStatus(t, h.do(t, "POST", "/v1/nodes/node-9/drain", `{}`), http.StatusNotFound)
	rec := h.do(t, "GET", "/v1/nodes/node-2/drain", "")
	wantStatus(t, rec, http.StatusNotFound)
	if !strings.Contains(errorOf(t, rec), "has no drain") {
		t.Fatalf("drain of a node without one: %s", rec.Body.String())
	}
	if n, _ := h.store.GetNode(ctx, "node-2"); !n.Eligible {
		t.Fatal("a bad request made the node ineligible")
	}

	rec = h.do(t, "POST", "/v1/nodes/node-2/eligibility", `{"eligible":false}`)
	wantStatus(t, rec, http.StatusOK)
	var el EligibilityReply
	if err := json.Unmarshal(rec.Body.Bytes(), &el); err != nil || el.ID != "node-2" || el.Name != "two" || el.Eligible || el.DrainCancelled {
		t.Fatalf("eligibility reply %+v, %v", el, err)
	}
	if nodes := nodesOf(t, h); nodes[1].Eligible || nodes[1].Replicas != 1 || nodes[1].Drain != nil {
		t.Fatalf("ineligible node: %+v", nodes[1])
	}

	before := h.trigger
	rec = h.do(t, "POST", "/v1/nodes/node-2/drain", `{"deadline":"10m"}`)
	wantStatus(t, rec, http.StatusOK)
	d := drainOf(t, rec)
	if d.NodeID != "node-2" || d.State != "draining" || d.Remaining != 1 || d.FinishedAt != nil || len(d.Blocked) != 0 ||
		len(d.Moving) != 1 || d.Moving[0].App != "web" || d.Moving[0].To != testNode {
		t.Fatalf("drain: %+v", d)
	}
	if got := d.Deadline.Sub(d.StartedAt); got != 10*time.Minute {
		t.Fatalf("deadline %s after the start, want 10m", got)
	}
	if h.trigger == before {
		t.Fatal("a drain did not trigger a reconcile")
	}
	// The JSON lists are never null.
	if strings.Contains(rec.Body.String(), "null") {
		t.Fatalf("drain body has a null: %s", rec.Body.String())
	}
	rec = h.do(t, "GET", "/v1/nodes/node-2/drain", "")
	wantStatus(t, rec, http.StatusOK)
	if got := drainOf(t, rec); got.State != "draining" || !got.Deadline.Equal(d.Deadline) || len(got.Moving) != 1 {
		t.Fatalf("GET drain: %+v", got)
	}
	nodes := nodesOf(t, h)
	if nodes[1].Drain == nil || nodes[1].Drain.State != "draining" || nodes[1].Eligible {
		t.Fatalf("nodes during the drain: %+v", nodes[1])
	}
	// An empty body keeps the deadline of the drain in progress.
	rec = h.do(t, "POST", "/v1/nodes/node-2/drain", `{}`)
	wantStatus(t, rec, http.StatusOK)
	if got := drainOf(t, rec); !got.Deadline.Equal(d.Deadline) || !got.StartedAt.Equal(d.StartedAt) {
		t.Fatalf("drain again: %+v, want deadline %s", got, d.Deadline)
	}

	// Making the node eligible cancels the drain.
	rec = h.do(t, "POST", "/v1/nodes/node-2/eligibility", `{"eligible":true}`)
	wantStatus(t, rec, http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &el); err != nil || !el.Eligible || !el.DrainCancelled {
		t.Fatalf("enable reply %+v, %v", el, err)
	}
	wantStatus(t, h.do(t, "GET", "/v1/nodes/node-2/drain", ""), http.StatusNotFound)
	logs := h.logs.String()
	for _, want := range []string{"node eligibility set", "node drain started", "drain_cancelled=true"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log has no %q", want)
		}
	}
}

// The server's own node can be drained. With no other node, nothing can
// move, and the drain says why.
func TestDrainTheServersNode(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", `{"name":"web","image":"nginx:alpine","replicas":1}`), http.StatusCreated)
	rec := h.do(t, "POST", "/v1/nodes/"+testNode+"/drain", `{}`)
	wantStatus(t, rec, http.StatusOK)
	d := drainOf(t, rec)
	if d.State != "draining" || d.Remaining != 1 || len(d.Moving) != 0 || len(d.Blocked) != 1 ||
		d.Blocked[0].Reason != "no other node is ready and eligible" || d.Deadline.Sub(d.StartedAt) != store.DefaultDrainDeadline {
		t.Fatalf("drain of the server's node: %+v", d)
	}
	// A new app waits: no node is eligible.
	wantStatus(t, h.do(t, "PUT", "/v1/apps/api", `{"name":"api","image":"busybox","replicas":1}`), http.StatusCreated)
	reps, err := h.store.Replicas(context.Background(), "api")
	if err != nil || len(reps) != 1 || reps[0].NodeID != "" || reps[0].PendingReason != "every ready node is ineligible for new replicas" {
		t.Fatalf("api on a cluster with no eligible node: %+v, %v", reps, err)
	}
}
