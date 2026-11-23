package router

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/metrics"
)

// unknownHost is the host label of requests for a host the table does not
// have, so that clients cannot grow the label set.
const unknownHost = "unknown"

// Metrics counts a router's requests and upstream errors. It is safe for
// concurrent use. A nil *Metrics counts nothing.
type Metrics struct {
	requests *metrics.CounterVec
	upstream *metrics.CounterVec
}

// NewMetrics returns empty counters.
func NewMetrics() *Metrics {
	return &Metrics{
		requests: metrics.NewCounterVec("host", "code"),
		upstream: metrics.NewCounterVec("host", "kind"),
	}
}

func (m *Metrics) request(host string, code int) {
	if m != nil {
		m.requests.Inc(host, strconv.Itoa(code))
	}
}

func (m *Metrics) upstreamError(host, kind string) {
	if m != nil {
		m.upstream.Inc(host, kind)
	}
}

// write adds the router's families to w. expiry is the expiry of the
// certificate each host serves now.
func (m *Metrics) write(w *metrics.Writer, expiry map[string]time.Time, hosts []string) {
	w.Family("hivemind_router_requests_total",
		"Requests to this node's router, by host and status code. Hosts not in the routing table count as unknown.", metrics.TypeCounter)
	if m != nil {
		m.requests.Write(w, "hivemind_router_requests_total")
	}
	w.Family("hivemind_router_upstream_errors_total",
		"Requests to a replica that failed, by host: dial (the router tried the next replica) or proxy (the request failed after it reached one).", metrics.TypeCounter)
	if m != nil {
		m.upstream.Write(w, "hivemind_router_upstream_errors_total")
	}
	w.Family("hivemind_router_cert_expiry_timestamp_seconds",
		"Unix time at which the certificate this router serves for the host expires. Hosts without a certificate yet are left out.", metrics.TypeGauge)
	for _, h := range hosts {
		if t, ok := expiry[h]; ok {
			w.Sample("hivemind_router_cert_expiry_timestamp_seconds", metrics.L("host", h), float64(t.Unix()))
		}
	}
}

// statusWriter records the status code a handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection, for flushing
// streamed replies and for upgrades.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Flush passes a flush on, for replies the proxy streams.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// counted wraps h so every request is counted by host and code. known says
// whether a host is in the table.
func counted(m *Metrics, known func(string) bool, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		h.ServeHTTP(sw, r)
		host := requestHost(r)
		if !known(host) {
			host = unknownHost
		}
		code := sw.code
		if code == 0 {
			code = http.StatusOK
		}
		m.request(host, code)
	})
}
