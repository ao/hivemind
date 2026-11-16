package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/store"
)

// The log of a replica that has no container yet says it is pending, and
// why, as status does, rather than "no container on this node".
func TestLogsOfAPendingReplica(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)
	rec := h.do(t, "GET", "/v1/apps/web/logs?replica=0", "")
	wantStatus(t, rec, http.StatusNotFound)
	if got := errorOf(t, rec); got != `app "web" replica 0 is pending on node `+testNode+
		", so it has no container to read a log from yet; hivemind status web shows when it runs" {
		t.Fatalf("a replica its node has not started: %q", got)
	}

	// The node reports why the replica is pending.
	if err := h.store.ReportStatus(context.Background(), testNode, []store.Status{{Name: "hivemind-web-0-g1", App: "web",
		Replica: 0, Generation: 1, State: "pending", Error: "pull: manifest unknown"}}); err != nil {
		t.Fatal(err)
	}
	rec = h.do(t, "GET", "/v1/apps/web/logs?replica=0", "")
	wantStatus(t, rec, http.StatusNotFound)
	if got := errorOf(t, rec); !strings.Contains(got, "yet: pull: manifest unknown; hivemind status web") {
		t.Fatalf("a replica whose pull fails: %q", got)
	}

	// A replica no node can take has no node and a reason.
	wantStatus(t, h.do(t, "PUT", "/v1/apps/big", `{"name":"big","image":"nginx:alpine","resources":{"cpu":64}}`), http.StatusCreated)
	rec = h.do(t, "GET", "/v1/apps/big/logs", "")
	wantStatus(t, rec, http.StatusNotFound)
	if got := errorOf(t, rec); !strings.HasPrefix(got, `app "big" replica 0 is pending, with no node, so it has no log yet: `) || strings.HasSuffix(got, ": ") {
		t.Fatalf("a replica with no node: %q", got)
	}
}

// The log of a replica past the app's count names the replicas the app has,
// rather than "no container on this node". One that a scale down is still
// removing is not past it: its log may still be read.
func TestLogsOfAReplicaPastTheCount(t *testing.T) {
	h := newHarness(t)
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)
	rec := h.do(t, "GET", "/v1/apps/web/logs?replica=7", "")
	wantStatus(t, rec, http.StatusNotFound)
	if got := errorOf(t, rec); got != `app "web" has replicas 0 to 1; it has no replica 7` {
		t.Fatalf("replica 7 of 2: %q", got)
	}
	wantStatus(t, h.do(t, "PUT", "/v1/apps/one", `{"name":"one","image":"nginx:alpine"}`), http.StatusCreated)
	rec = h.do(t, "GET", "/v1/apps/one/logs?replica=1", "")
	wantStatus(t, rec, http.StatusNotFound)
	if got := errorOf(t, rec); got != `app "one" has one replica, 0; it has no replica 1` {
		t.Fatalf("replica 1 of 1: %q", got)
	}

	wantStatus(t, h.do(t, "POST", "/v1/apps/web/scale", `{"replicas":1}`), http.StatusOK)
	reps, err := h.store.Replicas(context.Background(), "web")
	if err != nil || len(reps) != 2 || reps[1].Desired == store.Running {
		t.Fatalf("replicas after the scale down: %+v, %v; want replica 1 being removed", reps, err)
	}
	rec = h.do(t, "GET", "/v1/apps/web/logs?replica=1", "")
	if got := errorOf(t, rec); strings.Contains(got, "has no replica 1") {
		t.Fatalf("replica 1, being removed: %q", got)
	}
}
