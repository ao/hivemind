package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/metrics"
	"github.com/ao/hivemind/internal/store"
)

const routedWeb = `{"image":"nginx","replicas":1,"routes":[{"host":"www.example.com","port":80}]}`

// A PUT that claims a host another app routes gets 409, naming that app.
func TestPutAppHostClashIs409(t *testing.T) {
	h := newHarness(t)
	if rec := h.do(t, http.MethodPut, "/v1/apps/web", routedWeb); rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("put web: %d %s", rec.Code, rec.Body)
	}
	rec := h.do(t, http.MethodPut, "/v1/apps/api", `{"image":"nginx","routes":[{"host":"WWW.example.com","port":8080}]}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("put api: %d %s", rec.Code, rec.Body)
	}
	if msg := errorOf(t, rec); !strings.Contains(msg, "www.example.com") || !strings.Contains(msg, "app web") {
		t.Fatalf("409 message %q does not name the host and the app", msg)
	}
	if rec := h.do(t, http.MethodGet, "/v1/apps/api", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("the refused app exists: %d", rec.Code)
	}
	rec = h.do(t, http.MethodPut, "/v1/apps/bad", `{"image":"nginx","routes":[{"host":"*.example.com","port":80}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(errorOf(t, rec), "wildcard") {
		t.Fatalf("wildcard host: %d %s", rec.Code, rec.Body)
	}
}

func TestListRoutes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.store.UpsertNode(ctx, store.Node{ID: testNode, Name: "test", CPU: 2, Memory: 1 << 30, Address: "10.0.0.5"}); err != nil {
		t.Fatal(err)
	}
	if rec := h.do(t, http.MethodPut, "/v1/apps/web", routedWeb); rec.Code/100 != 2 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if err := h.store.ReportStatus(ctx, testNode, []store.Status{{Name: "hivemind-web-0-g1", App: "web", Generation: 1,
		State: "running", Ready: true, RoutePorts: []store.RoutePort{{Port: 80, HostPort: 32768}}}}); err != nil {
		t.Fatal(err)
	}
	rec := h.do(t, http.MethodGet, "/v1/routes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/routes: %d %s", rec.Code, rec.Body)
	}
	var got RouteList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Routes) != 1 || got.Routes[0].App != "web" || got.Routes[0].Port != 80 ||
		!slices.Equal(got.Routes[0].Endpoints, []string{"10.0.0.5:32768"}) {
		t.Fatalf("routes: %+v", got)
	}
	rec = h.do(t, http.MethodGet, "/v1/nodes", "")
	if !strings.Contains(rec.Body.String(), `"address":"10.0.0.5"`) {
		t.Fatalf("GET /v1/nodes has no address: %s", rec.Body)
	}
}

func TestRouterMetricsAreServed(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.RouterMetrics = func(w *metrics.Writer) {
			w.Family("hivemind_router_requests_total", "test", metrics.TypeCounter)
			w.Sample("hivemind_router_requests_total", metrics.L("host", "www.example.com", "code", "200"), 3)
		}
	})
	rec := h.do(t, http.MethodGet, "/metrics", "")
	if !strings.Contains(rec.Body.String(), `hivemind_router_requests_total{host="www.example.com",code="200"} 3`) {
		t.Fatalf("router metrics missing:\n%s", rec.Body)
	}
}
