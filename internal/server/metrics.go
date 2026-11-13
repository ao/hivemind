package server

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/metrics"
	"github.com/ao/hivemind/internal/store"
)

// reconcileBuckets are the upper bounds, in seconds, of the reconcile pass
// histogram. A pass with nothing to do takes milliseconds; one that pulls an
// image can take a minute.
var reconcileBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Metrics holds the counters that are not read from the store at scrape
// time: HTTP requests and reconcile passes. One Metrics is shared by the
// API, the agent and the metrics listener. It is safe for concurrent use.
type Metrics struct {
	requests        *metrics.CounterVec
	reconcile       *metrics.Histogram
	reconcileErrors *metrics.CounterVec
	now             func() time.Time
}

// NewMetrics returns empty counters.
func NewMetrics() *Metrics {
	return &Metrics{
		requests:        metrics.NewCounterVec("method", "route", "code"),
		reconcile:       metrics.NewHistogram(reconcileBuckets...),
		reconcileErrors: metrics.NewCounterVec(),
		now:             time.Now,
	}
}

// ObserveReconcile records one agent pass. Pass it as agent.Config.OnPass.
func (m *Metrics) ObserveReconcile(d time.Duration, err error) {
	m.reconcile.Observe(d.Seconds())
	if err != nil {
		m.reconcileErrors.Inc()
	}
}

// observeRequest counts one request. route is the matched pattern, so the
// label set stays bounded whatever paths clients send.
func (m *Metrics) observeRequest(method, route string, code int) {
	m.requests.Inc(methodLabel(method), route, strconv.Itoa(code))
}

// methodLabel keeps the method label to the methods HTTP defines.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return m
	}
	return "other"
}

// routeLabel is the route pattern without its method, such as
// /v1/apps/{name}, or "unmatched" for a request no route takes.
func routeLabel(router *auth.Router, r *http.Request) string {
	p := router.Match(r)
	if p == "" {
		return "unmatched"
	}
	if _, path, ok := strings.Cut(p, " "); ok {
		return path
	}
	return p
}

// metricsHandler serves GET /metrics. Cluster state is read from the store
// on each scrape, so the values are never older than the store.
func (s *Server) metricsHandler(w http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	if err := s.writeMetrics(r, &b); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", metrics.ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
}

func (s *Server) writeMetrics(r *http.Request, b *bytes.Buffer) error {
	ctx := r.Context()
	st := s.cfg.Store
	apps, err := st.ListApps(ctx)
	if err != nil {
		return err
	}
	reps, err := st.Replicas(ctx, "")
	if err != nil {
		return err
	}
	sts, err := st.ListStatus(ctx, "")
	if err != nil {
		return err
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		return err
	}

	type key struct {
		app      string
		idx, gen int
	}
	byKey := map[key]store.Status{}
	for _, x := range sts {
		byKey[key{x.App, x.Replica, x.Generation}] = x
	}
	type counts struct{ running, ready int }
	perApp := map[string]*counts{}
	for _, a := range apps {
		perApp[a.Spec.Name] = &counts{}
	}
	type restart struct {
		app     string
		replica int
		n       int
	}
	var restarts []restart
	for _, rp := range reps {
		c := perApp[rp.App]
		if c == nil || rp.Desired != store.Running {
			continue
		}
		x, ok := byKey[key{rp.App, rp.Index, rp.Generation}]
		if !ok {
			continue
		}
		if x.State == "running" {
			c.running++
		}
		if x.Ready {
			c.ready++
		}
		restarts = append(restarts, restart{rp.App, rp.Index, x.RestartCount})
	}

	w := metrics.NewWriter(b)
	w.Family("hivemind_apps", "Apps in the cluster store.", metrics.TypeGauge)
	w.Sample("hivemind_apps", nil, float64(len(apps)))

	w.Family("hivemind_replicas_desired", "Replicas the app spec asks for.", metrics.TypeGauge)
	for _, a := range apps {
		w.Sample("hivemind_replicas_desired", metrics.L("app", a.Spec.Name), float64(a.Spec.Replicas))
	}
	w.Family("hivemind_replicas_running", "Replicas whose container at the current generation is running.", metrics.TypeGauge)
	for _, a := range apps {
		w.Sample("hivemind_replicas_running", metrics.L("app", a.Spec.Name), float64(perApp[a.Spec.Name].running))
	}
	w.Family("hivemind_replicas_ready", "Replicas whose container at the current generation is running and passes its health check.", metrics.TypeGauge)
	for _, a := range apps {
		w.Sample("hivemind_replicas_ready", metrics.L("app", a.Spec.Name), float64(perApp[a.Spec.Name].ready))
	}
	w.Family("hivemind_container_restarts_total",
		"Restarts of the replica's current container by the agent. It resets after 10 minutes of healthy running.", metrics.TypeCounter)
	for _, x := range restarts {
		w.Sample("hivemind_container_restarts_total", metrics.L("app", x.app, "replica", strconv.Itoa(x.replica)), float64(x.n))
	}

	w.Family("hivemind_node_lease_age_seconds", "Seconds since the node last renewed its lease.", metrics.TypeGauge)
	now := s.metrics.now()
	for _, n := range nodes {
		age := now.Sub(n.LastLease).Seconds()
		if age < 0 {
			age = 0
		}
		w.Sample("hivemind_node_lease_age_seconds", metrics.L("node", n.ID), age)
	}

	w.Family("hivemind_reconcile_duration_seconds", "Wall time of the local agent's reconcile passes.", metrics.TypeHistogram)
	s.metrics.reconcile.Write(w, "hivemind_reconcile_duration_seconds")
	w.Family("hivemind_reconcile_errors_total", "Reconcile passes of the local agent that returned an error.", metrics.TypeCounter)
	w.Sample("hivemind_reconcile_errors_total", nil, s.metrics.reconcileErrors.Get())

	w.Family("hivemind_http_requests_total", "HTTP requests by method, route pattern and status code.", metrics.TypeCounter)
	s.metrics.requests.Write(w, "hivemind_http_requests_total")
	return w.Flush()
}
