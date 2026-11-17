package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/spec"
)

// A worker posts its router's counters; the server keeps those for hosts it
// routes, and the unknown host's request counts, under the worker's node ID.
func TestRouterMetricsOverTheNodeAPI(t *testing.T) {
	reports := nodeapi.NewRouterReports()
	fx := newFixtureWith(t, func(c *Config) { c.RouterReports = reports })
	ctx := context.Background()
	idA, certA := fx.enroll(fx.issue(time.Hour))
	idB, certB := fx.enroll(fx.issue(time.Hour))
	fx.registerNode(fx.nodeClient(certA), idA)
	fx.registerNode(fx.nodeClient(certB), idB)
	if _, err := fx.st.PutApp(ctx, spec.App{Name: "web", Image: "nginx", Replicas: 1,
		Routes: []spec.Route{{Host: "www.example.com", Port: 80}}}); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(60 * 24 * time.Hour).UTC().Truncate(time.Second)
	rep := nodeapi.RouterReport{
		Requests: []nodeapi.RouterRequests{
			{Host: "www.example.com", Code: 200, Count: 7},
			{Host: nodeapi.UnknownHost, Code: 404, Count: 2},
			{Host: "not-routed.example.com", Code: 200, Count: 1},
		},
		Upstream: []nodeapi.RouterUpstream{
			{Host: "www.example.com", Kind: "dial", Count: 1},
			{Host: "not-routed.example.com", Kind: "proxy", Count: 5},
		},
		Certs: []nodeapi.RouterCert{
			{Host: "www.example.com", NotAfter: expiry},
			{Host: "not-routed.example.com", NotAfter: expiry},
		},
	}
	if err := fx.apiClient(certA).ReportRouter(ctx, idA, rep); err != nil {
		t.Fatal(err)
	}
	got := reports.Fresh(time.Now(), time.Minute)
	if len(got) != 1 {
		t.Fatalf("reports: %+v", got)
	}
	a := got[idA]
	if len(a.Requests) != 2 || a.Requests[0].Count != 7 || a.Requests[1].Host != nodeapi.UnknownHost {
		t.Errorf("requests kept: %+v", a.Requests)
	}
	if len(a.Upstream) != 1 || a.Upstream[0].Kind != "dial" {
		t.Errorf("upstream kept: %+v", a.Upstream)
	}
	if len(a.Certs) != 1 || !a.Certs[0].NotAfter.Equal(expiry) {
		t.Errorf("certs kept: %+v", a.Certs)
	}

	// A node cannot post for another node.
	b, _ := json.Marshal(rep)
	if code, _ := fx.send(fx.nodeClient(certA), http.MethodPost, fx.nodeURL(nodeapi.RouterMetricsPath(idB)), b); code != http.StatusForbidden {
		t.Errorf("A posting on B's path: %d, want 403", code)
	}
	// Malformed reports are refused and keep nothing.
	for name, bad := range map[string]nodeapi.RouterReport{
		"status code": {Requests: []nodeapi.RouterRequests{{Host: "www.example.com", Code: 42, Count: 1}}},
		"host":        {Requests: []nodeapi.RouterRequests{{Host: "Not A Host", Code: 200, Count: 1}}},
		"kind":        {Upstream: []nodeapi.RouterUpstream{{Host: "www.example.com", Kind: "other", Count: 1}}},
		"expiry":      {Certs: []nodeapi.RouterCert{{Host: "www.example.com"}}},
		"cert host":   {Certs: []nodeapi.RouterCert{{Host: nodeapi.UnknownHost, NotAfter: expiry}}},
	} {
		body, _ := json.Marshal(bad)
		if code, _ := fx.send(fx.nodeClient(certB), http.MethodPost, fx.nodeURL(nodeapi.RouterMetricsPath(idB)), body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	huge := []byte(`{"requests":[{"host":"` + strings.Repeat("a", nodeapi.MaxRouterReportBytes) + `"}]}`)
	if code, _ := fx.send(fx.nodeClient(certB), http.MethodPost, fx.nodeURL(nodeapi.RouterMetricsPath(idB)), huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("body over the limit: %d, want 413", code)
	}
	if _, ok := reports.Fresh(time.Now(), time.Minute)[idB]; ok {
		t.Error("a refused report was kept")
	}
}

// Without a holder the route answers, and drops the report.
func TestRouterMetricsWithoutAHolder(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	fx.registerNode(fx.nodeClient(cert), id)
	if err := fx.apiClient(cert).ReportRouter(context.Background(), id, nodeapi.RouterReport{}); err != nil {
		t.Fatal(err)
	}
}
