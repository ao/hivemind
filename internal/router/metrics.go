package router

import (
	"maps"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/metrics"
	"github.com/ao/hivemind/internal/nodeapi"
)

// unknownHost is the host label of requests for a host the table does not
// have, so that clients cannot grow the label set.
const unknownHost = nodeapi.UnknownHost

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

// report is the counters and certificate expiries as a node API report.
func (m *Metrics) report(expiry map[string]time.Time) nodeapi.RouterReport {
	var r nodeapi.RouterReport
	if m != nil {
		for _, s := range m.requests.Snapshot() {
			code, err := strconv.Atoi(s.Values[1])
			if err != nil {
				continue
			}
			r.Requests = append(r.Requests, nodeapi.RouterRequests{Host: s.Values[0], Code: code, Count: nodeapi.CountOf(s.N)})
		}
		for _, s := range m.upstream.Snapshot() {
			r.Upstream = append(r.Upstream, nodeapi.RouterUpstream{Host: s.Values[0], Kind: s.Values[1], Count: nodeapi.CountOf(s.N)})
		}
	}
	for _, h := range slices.Sorted(maps.Keys(expiry)) {
		r.Certs = append(r.Certs, nodeapi.RouterCert{Host: h, NotAfter: expiry[h].UTC()})
	}
	return r
}
