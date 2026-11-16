package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/store"
)

// Every authenticated reply carries the server's version, GET /v1/status
// included. GET /healthz, which needs no token, and a refused request do
// not.
func TestServerVersionHeader(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Version = "v2.2.0" })
	for _, path := range []string{"/v1/status", "/v1/nodes", "/v1/apps/nope", "/v1/no-such-route"} {
		rec := h.do(t, "GET", path, "")
		if got := rec.Header().Get(nodeapi.VersionHeader); got != "v2.2.0" {
			t.Errorf("GET %s (%d): %s %q, want v2.2.0", path, rec.Code, nodeapi.VersionHeader, got)
		}
	}
	for name, req := range map[string]*http.Request{
		"healthz":     httptest.NewRequest("GET", "/healthz", nil),
		"no token":    httptest.NewRequest("GET", "/v1/status", nil),
		"wrong token": withBearer(httptest.NewRequest("GET", "/v1/status", nil), "hm_wrong"),
	} {
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		if got := rec.Header().Get(nodeapi.VersionHeader); got != "" {
			t.Errorf("%s (%d): %s %q, want none", name, rec.Code, nodeapi.VersionHeader, got)
		}
	}
}

func withBearer(r *http.Request, tok string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

// GET /v1/nodes has each node's version, and none for a node that reported
// none.
func TestNodesListVersions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.UpsertNode(ctx, store.Node{ID: "node-new", Name: "new", Version: "v2.2.0"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertNode(ctx, store.Node{ID: "node-old", Name: "old"}); err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, "GET", "/v1/nodes", "")
	wantStatus(t, rec, http.StatusOK)
	var nodes []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, n := range nodes {
		got[n["id"].(string)] = n["version"]
	}
	if got["node-new"] != "v2.2.0" || got["node-old"] != nil {
		t.Fatalf("versions %v", got)
	}
}

// A log request a worker does not take says what is known about the worker:
// one that reports no version and has not polled since the server started
// is older than 2.1 and cannot stream logs; one that reports a version is
// named with it.
func TestRemoteLogsNamesAnOldWorker(t *testing.T) {
	checkGoroutines(t)
	old := neverPolledAfter
	neverPolledAfter = 0
	t.Cleanup(func() { neverPolledAfter = old })

	rh := newRemoteHarness(t, shortRelay(), nil)
	rec := rh.logs(t, "")
	wantStatus(t, rec, http.StatusGatewayTimeout)
	if got := errorOf(t, rec); got != "worker "+workerNode+" runs a hivemind older than 2.1, which cannot stream logs; upgrade it" {
		t.Fatalf("a worker that never polled: %q", got)
	}

	if err := rh.store.SetNodeVersion(context.Background(), workerNode, "v2.2.0"); err != nil {
		t.Fatal(err)
	}
	rec = rh.logs(t, "")
	wantStatus(t, rec, http.StatusGatewayTimeout)
	if got := errorOf(t, rec); !strings.Contains(got, "worker "+workerNode+", which runs v2.2.0, did not answer") {
		t.Fatalf("a worker with a version: %q", got)
	}

	// A worker that has polled can stream logs, whatever its version.
	if err := rh.store.SetNodeVersion(context.Background(), workerNode, ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	_, _ = rh.relay.Next(ctx, workerNode)
	cancel()
	rec = rh.logs(t, "")
	wantStatus(t, rec, http.StatusGatewayTimeout)
	if got := errorOf(t, rec); !strings.Contains(got, "did not answer") || strings.Contains(got, "older") {
		t.Fatalf("a worker that polled: %q", got)
	}
}
