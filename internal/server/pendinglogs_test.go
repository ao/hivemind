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
