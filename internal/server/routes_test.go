package server

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
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
	reports := nodeapi.NewRouterReports()
	now := time.Now()
	reports.Put("node-w", nodeapi.RouterReport{
		Requests: []nodeapi.RouterRequests{{Host: "www.example.com", Code: 502, Count: 4}},
		Upstream: []nodeapi.RouterUpstream{{Host: "www.example.com", Kind: "dial", Count: 2}},
	}, now)
	// A worker that stopped reporting drops out.
	reports.Put("node-gone", nodeapi.RouterReport{
		Requests: []nodeapi.RouterRequests{{Host: "www.example.com", Code: 200, Count: 9}},
	}, now.Add(-routerReportAge-time.Second))
	expiry := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	h := newHarness(t, func(c *Config) {
		c.RouterReports = reports
		c.RouterReport = func() nodeapi.RouterReport {
			return nodeapi.RouterReport{
				Requests: []nodeapi.RouterRequests{{Host: "www.example.com", Code: 200, Count: 3}},
				Certs:    []nodeapi.RouterCert{{Host: "www.example.com", NotAfter: expiry}},
			}
		}
	})
	body := h.do(t, http.MethodGet, "/metrics", "").Body.String()
	for _, want := range []string{
		`hivemind_router_requests_total{node="` + testNode + `",host="www.example.com",code="200"} 3`,
		`hivemind_router_requests_total{node="node-w",host="www.example.com",code="502"} 4`,
		`hivemind_router_upstream_errors_total{node="node-w",host="www.example.com",kind="dial"} 2`,
		`hivemind_router_cert_expiry_timestamp_seconds{node="` + testNode + `",host="www.example.com"} ` + strconv.FormatInt(expiry.Unix(), 10),
		"# TYPE hivemind_router_requests_total counter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics has no %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "node-gone") {
		t.Errorf("a stale report is shown:\n%s", body)
	}
	if n := strings.Count(body, "# TYPE hivemind_router_requests_total"); n != 1 {
		t.Errorf("%d request families, want 1", n)
	}
}

// Without a router on any node, /metrics has no router families.
func TestNoRouterNoRouterMetrics(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.RouterReports = nodeapi.NewRouterReports() })
	if body := h.do(t, http.MethodGet, "/metrics", "").Body.String(); strings.Contains(body, "hivemind_router_") {
		t.Fatalf("router families without a router:\n%s", body)
	}
}

// A worker's report alone, with no router on the server, is shown.
func TestWorkerRouterMetricsWithoutALocalRouter(t *testing.T) {
	reports := nodeapi.NewRouterReports()
	reports.Put("node-w", nodeapi.RouterReport{Requests: []nodeapi.RouterRequests{{Host: nodeapi.UnknownHost, Code: 404, Count: 1}}}, time.Now())
	h := newHarness(t, func(c *Config) { c.RouterReports = reports })
	body := h.do(t, http.MethodGet, "/metrics", "").Body.String()
	if !strings.Contains(body, `hivemind_router_requests_total{node="node-w",host="unknown",code="404"} 1`) {
		t.Fatalf("worker series missing:\n%s", body)
	}
}
