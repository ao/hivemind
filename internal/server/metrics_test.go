package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/metrics"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// oddNode needs every escape the format has: a quote, a backslash and a
// line feed. Node IDs are not restricted the way app names are.
const oddNode = "zz \"quoted\" \\ node\nline2"

type metricsEnv struct {
	srv   *Server
	st    *store.Store
	m     *Metrics
	token string
}

func newMetricsEnv(t *testing.T) *metricsEnv {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	// The checks expect every replica on testNode, so placement is pinned to
	// the first node rather than spread by the scheduler.
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hivemind.db"), store.WithClock(func() time.Time { return t0 }),
		store.WithMinReady(0), store.WithPlacement(store.FirstNode))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, id := range []string{testNode, oddNode} {
		if err := st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 2, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	tok, _ := auth.NewToken()
	sessions, _ := auth.NewSessions(time.Hour)
	a, _ := auth.NewAuthenticator(auth.HashToken(tok), sessions)
	m := NewMetrics()
	m.now = func() time.Time { return t0.Add(42500 * time.Millisecond) }
	srv, err := New(Config{Store: st, Runtime: &fakeRuntime{}, NodeID: testNode, Auth: a, Metrics: m,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return &metricsEnv{srv: srv, st: st, m: m, token: tok}
}

func (e *metricsEnv) get(h http.Handler, path string, token bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (e *metricsEnv) scrape(t *testing.T, h http.Handler) (map[string]float64, string) {
	t.Helper()
	rec := e.get(h, "/metrics", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != metrics.ContentType {
		t.Fatalf("content type %q", ct)
	}
	text := rec.Body.String()
	got, err := metrics.Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("exposition does not parse: %v\n%s", err, text)
	}
	return got, text
}

// TestMetricsMatchStore builds a known cluster state and checks every
// scraped value against it.
func TestMetricsMatchStore(t *testing.T) {
	e := newMetricsEnv(t)
	ctx := context.Background()
	if _, err := e.st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 3,
		Env: map[string]string{"SECRET": "env-secret-value"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.PutApp(ctx, spec.App{Name: "api", Image: "ghcr.io/x/api:1", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.PutApp(ctx, spec.App{Name: "gone", Image: "busybox", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteApp(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	reps, _ := e.st.Replicas(ctx, "web")
	for _, r := range reps {
		if r.NodeID != testNode {
			t.Fatalf("replica %+v not on %s", r, testNode)
		}
	}
	err := e.st.ReportStatus(ctx, testNode, []store.Status{
		{Name: "hivemind-web-0-g1", ContainerID: "c0", App: "web", Replica: 0, Generation: 1, State: "running", Ready: true, RestartCount: 2},
		{Name: "hivemind-web-1-g1", ContainerID: "c1", App: "web", Replica: 1, Generation: 1, State: "running", Health: "starting"},
		{Name: "hivemind-web-2-g1", ContainerID: "c2", App: "web", Replica: 2, Generation: 1, State: "exited", RestartCount: 5, CrashLooping: true},
		// An old generation's container does not count for the replica.
		{Name: "hivemind-web-2-g0", ContainerID: "c9", App: "web", Replica: 2, Generation: 0, State: "running", Ready: true, RestartCount: 7},
		{Name: "hivemind-gone-0-g1", ContainerID: "c3", App: "gone", Replica: 0, Generation: 1, State: "running", Ready: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.m.ObserveReconcile(30*time.Millisecond, nil)
	e.m.ObserveReconcile(2*time.Second, errors.New("boom"))

	h := e.srv.Handler()
	for _, p := range []string{"/v1/apps", "/v1/apps/web", "/v1/apps/api", "/v1/apps/nope", "/v1/apps/also-nope"} {
		e.get(h, p, true)
	}
	e.get(h, "/metrics", false)
	e.get(h, "/v1/no/such/route", false)
	e.get(h, "/healthz", false)

	got, text := e.scrape(t, h)
	want := map[string]float64{
		metrics.Key("hivemind_apps"):                                                                            2,
		metrics.Key("hivemind_replicas_desired", "app", "web"):                                                  3,
		metrics.Key("hivemind_replicas_desired", "app", "api"):                                                  1,
		metrics.Key("hivemind_replicas_running", "app", "web"):                                                  2,
		metrics.Key("hivemind_replicas_running", "app", "api"):                                                  0,
		metrics.Key("hivemind_replicas_ready", "app", "web"):                                                    1,
		metrics.Key("hivemind_replicas_ready", "app", "api"):                                                    0,
		metrics.Key("hivemind_container_restarts_total", "app", "web", "replica", "0"):                          2,
		metrics.Key("hivemind_container_restarts_total", "app", "web", "replica", "1"):                          0,
		metrics.Key("hivemind_container_restarts_total", "app", "web", "replica", "2"):                          5,
		metrics.Key("hivemind_node_lease_age_seconds", "node", testNode):                                        42.5,
		metrics.Key("hivemind_node_lease_age_seconds", "node", oddNode):                                         42.5,
		metrics.Key("hivemind_reconcile_duration_seconds_bucket", "le", "0.025"):                                0,
		metrics.Key("hivemind_reconcile_duration_seconds_bucket", "le", "0.05"):                                 1,
		metrics.Key("hivemind_reconcile_duration_seconds_bucket", "le", "1"):                                    1,
		metrics.Key("hivemind_reconcile_duration_seconds_bucket", "le", "2.5"):                                  2,
		metrics.Key("hivemind_reconcile_duration_seconds_bucket", "le", "+Inf"):                                 2,
		metrics.Key("hivemind_reconcile_duration_seconds_sum"):                                                  2.03,
		metrics.Key("hivemind_reconcile_duration_seconds_count"):                                                2,
		metrics.Key("hivemind_reconcile_errors_total"):                                                          1,
		metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "/v1/apps", "code", "200"):        1,
		metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "/v1/apps/{name}", "code", "200"): 2,
		metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "/v1/apps/{name}", "code", "404"): 2,
		metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "/metrics", "code", "401"):        1,
		metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "unmatched", "code", "401"):       1,
		metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "/healthz", "code", "200"):        1,
	}
	for k, v := range want {
		g, ok := got[k]
		if !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	// Nothing for the deleted app, and no series per raw path.
	for k := range got {
		if strings.Contains(k, `"gone"`) || strings.Contains(k, "nope") {
			t.Errorf("unexpected series %s", k)
		}
	}
	if n := countPrefix(got, "hivemind_http_requests_total"); n != 6 {
		t.Errorf("%d request series, want 6:\n%s", n, text)
	}
	// Every family has HELP and TYPE, and the odd node ID is escaped on one line.
	for _, fam := range []string{"hivemind_apps", "hivemind_replicas_desired", "hivemind_replicas_running", "hivemind_replicas_ready",
		"hivemind_container_restarts_total", "hivemind_node_lease_age_seconds", "hivemind_reconcile_duration_seconds",
		"hivemind_reconcile_errors_total", "hivemind_http_requests_total"} {
		if !strings.Contains(text, "# HELP "+fam+" ") || !strings.Contains(text, "# TYPE "+fam+" ") {
			t.Errorf("family %s lacks HELP or TYPE", fam)
		}
	}
	if !strings.Contains(text, `node="zz \"quoted\" \\ node\nline2"`) {
		t.Errorf("node label not escaped:\n%s", text)
	}
	if strings.Contains(text, "env-secret-value") {
		t.Error("an env value reached the metrics")
	}

	// The scrape itself is counted on the next one.
	got, _ = e.scrape(t, h)
	if v := got[metrics.Key("hivemind_http_requests_total", "method", "GET", "route", "/metrics", "code", "200")]; v != 1 {
		t.Errorf("scrapes counted %v, want 1", v)
	}
}

func countPrefix(m map[string]float64, prefix string) int {
	n := 0
	for k := range m {
		if strings.HasPrefix(k, prefix+"{") || k == prefix {
			n++
		}
	}
	return n
}

// TestMetricsNeedToken checks /metrics on the API handler and on the
// separate metrics handler: 401 without the token or with a wrong one, 200
// with it.
func TestMetricsNeedToken(t *testing.T) {
	e := newMetricsEnv(t)
	if !slices.Contains(e.srv.Patterns(), "GET /metrics") {
		t.Fatalf("GET /metrics is not a registered API route: %v", e.srv.Patterns())
	}
	if got := e.srv.MetricsPatterns(); !slices.Equal(got, []string{"GET /metrics"}) {
		t.Fatalf("metrics listener routes: %v", got)
	}
	other, _ := auth.NewToken()
	for name, h := range map[string]http.Handler{"api": e.srv.Handler(), "metrics listener": e.srv.MetricsHandler()} {
		if rec := e.get(h, "/metrics", false); rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s: /metrics without a token: %d", name, rec.Code)
		}
		req := httptest.NewRequest("GET", "/metrics", nil)
		req.Header.Set("Authorization", "Bearer "+other)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: /metrics with a wrong token: %d", name, rec.Code)
		}
		if rec := e.get(h, "/metrics", true); rec.Code != http.StatusOK {
			t.Errorf("%s: /metrics with the token: %d", name, rec.Code)
		}
	}
	// The metrics listener serves nothing else, with or without the token.
	mh := e.srv.MetricsHandler()
	for _, p := range []string{"/v1/apps", "/v1/status"} {
		if rec := e.get(mh, p, true); rec.Code != http.StatusNotFound {
			t.Errorf("metrics listener %s with the token: %d, want 404", p, rec.Code)
		}
		if rec := e.get(mh, p, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("metrics listener %s without the token: %d, want 401", p, rec.Code)
		}
	}
}

// TestMetricsStoreError checks that a store failure gives 500, not a
// partial exposition.
func TestMetricsStoreError(t *testing.T) {
	e := newMetricsEnv(t)
	e.st.Close()
	rec := e.get(e.srv.Handler(), "/metrics", true)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "hivemind_apps") {
		t.Fatalf("closed store: %d %s", rec.Code, rec.Body.String())
	}
}
