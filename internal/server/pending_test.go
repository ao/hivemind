package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

const heldByAPI = "host port 127.0.0.1:8080/tcp is in use on every eligible node; held by app api"

// The reply to a put lists the replicas no node took, with the reason status
// shows and the app that holds the port, and says which reasons last. A put
// that places everything has no pending key.
func TestPutListsPendingReplicas(t *testing.T) {
	h := newHarness(t) // one node: 2 CPUs, 1Gi
	put := func(path, body string, code int) (PutResult, string) {
		t.Helper()
		rec := h.do(t, "PUT", path, body)
		wantStatus(t, rec, code)
		var pr PutResult
		if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
			t.Fatal(err)
		}
		return pr, rec.Body.String()
	}
	if _, body := put("/v1/apps/api", `{"image":"nginx:alpine","ports":[{"host":8080,"container":80}]}`, http.StatusCreated); strings.Contains(body, "pending") {
		t.Fatalf("a put that placed everything: %s", body)
	}
	pr, _ := put("/v1/apps/web", `{"image":"nginx:alpine","ports":[{"host":8080,"container":80}]}`, http.StatusCreated)
	if want := []PendingReplica{{Replica: 0, Reason: heldByAPI, Stuck: true}}; !reflect.DeepEqual(pr.Pending, want) || pr.PendingCount != 1 {
		t.Fatalf("port held by api: %+v (%d), want %+v", pr.Pending, pr.PendingCount, want)
	}
	// Status shows the same reason, without the app.
	rec := h.do(t, "GET", "/v1/apps/web/status", "")
	wantStatus(t, rec, http.StatusOK)
	var st AppStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Replicas) != 1 || !strings.HasPrefix(heldByAPI, st.Replicas[0].PendingReason+"; held by") {
		t.Fatalf("status: %+v", st.Replicas)
	}

	pr, _ = put("/v1/apps/big", `{"image":"nginx:alpine","resources":{"memory":"4Gi"}}`, http.StatusCreated)
	if want := []PendingReplica{{Replica: 0, Reason: "no node has 4Gi of memory free", Stuck: true}}; !reflect.DeepEqual(pr.Pending, want) {
		t.Fatalf("too much memory: %+v", pr.Pending)
	}
	pr, _ = put("/v1/apps/addr", `{"image":"nginx:alpine","ports":[{"bind":"10.9.9.9","host":5432,"container":5432}]}`, http.StatusCreated)
	if len(pr.Pending) != 1 || !pr.Pending[0].Stuck || !strings.HasPrefix(pr.Pending[0].Reason, "no node has address 10.9.9.9") {
		t.Fatalf("a bind address no node has: %+v", pr.Pending)
	}

	// An update that still cannot be placed says so again; one that can,
	// does not.
	pr, _ = put("/v1/apps/web", `{"image":"nginx:alpine","ports":[{"host":8080,"container":80}]}`, http.StatusOK)
	if !pr.Unchanged || len(pr.Pending) != 1 {
		t.Fatalf("the same spec again: %+v", pr)
	}
	if pr, body := put("/v1/apps/web", `{"image":"nginx:alpine","ports":[{"host":8081,"container":80}]}`, http.StatusOK); pr.Pending != nil || strings.Contains(body, "pending") {
		t.Fatalf("a free port: %s", body)
	}
}

// The list stops at MaxPendingListed, and the count has them all.
func TestPutBoundsThePendingList(t *testing.T) {
	h := newHarness(t)
	rec := h.do(t, "PUT", "/v1/apps/many", `{"image":"nginx:alpine","replicas":14,"ports":[{"host":9090,"container":80}]}`)
	wantStatus(t, rec, http.StatusCreated)
	var pr PutResult
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
		t.Fatal(err)
	}
	if len(pr.Pending) != MaxPendingListed || pr.PendingCount != 13 {
		t.Fatalf("%d listed, count %d; want %d and 13", len(pr.Pending), pr.PendingCount, MaxPendingListed)
	}
	for i, p := range pr.Pending {
		if p.Replica != i+1 || !p.Stuck || p.Reason != "host port 127.0.0.1:9090/tcp is in use on every eligible node; held by another replica of many" {
			t.Fatalf("pending[%d] = %+v", i, p)
		}
	}
}

// The reply to a scale is the app, as before, with the new replicas no node
// took.
func TestScaleListsPendingReplicas(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/api", `{"image":"nginx:alpine","ports":[{"host":8080,"container":80}]}`), http.StatusCreated)
	rec := h.do(t, "POST", "/v1/apps/api/scale", `{"replicas":3}`)
	wantStatus(t, rec, http.StatusOK)
	var sr ScaleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	held := "host port 127.0.0.1:8080/tcp is in use on every eligible node; held by another replica of api"
	want := []PendingReplica{{Replica: 1, Reason: held, Stuck: true}, {Replica: 2, Reason: held, Stuck: true}}
	if sr.Name != "api" || sr.Replicas != 3 || !reflect.DeepEqual(sr.Pending, want) || sr.PendingCount != 2 {
		t.Fatalf("scale: %s", rec.Body)
	}
	// An older client reads the same reply as an AppView.
	var v AppView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Name != "api" || v.Replicas != 3 {
		t.Fatalf("as an AppView: %+v, %v", v, err)
	}
	rec = h.do(t, "POST", "/v1/apps/api/scale", `{"replicas":1}`)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "pending") {
		t.Fatalf("scale down: %s", rec.Body)
	}
}
