package nodeapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// RouterMetricsPath is the route a worker posts its router's counters to. A
// worker has no metrics listener, so the server's /metrics shows them.
func RouterMetricsPath(id string) string { return Prefix + "/nodes/" + id + "/router-metrics" }

// MaxRouterReportBytes limits a router report's body.
const MaxRouterReportBytes = 4 << 20

// maxRouterSeries limits the series in one report, all kinds together.
const maxRouterSeries = 50_000

// UnknownHost is the host of a router's requests for hosts not in its
// routing table, as the router counts them.
const UnknownHost = "unknown"

// The kinds of a router's upstream errors.
var routerErrorKinds = []string{"dial", "connection", "proxy"}

// The reasons a router refuses a connection: its client address has as many
// open as the per-address cap allows, or the router has as many as its total
// cap.
var routerRefusedReasons = []string{"address", "total"}

// RouterReport is a router's counters since it started, and the expiry of
// the certificate it serves for each host.
type RouterReport struct {
	Requests []RouterRequests `json:"requests"`
	Upstream []RouterUpstream `json:"upstream"`
	Certs    []RouterCert     `json:"certs"`
	// Refused is left out when empty, as it was before there was one.
	Refused []RouterRefused `json:"refused,omitempty"`
}

// RouterRefused counts the connections a router closed at accept because
// they were over one of its caps.
type RouterRefused struct {
	Reason string `json:"reason"`
	Count  uint64 `json:"count"`
}

// RouterRequests counts a router's requests for one host with one status.
type RouterRequests struct {
	Host  string `json:"host"`
	Code  int    `json:"code"`
	Count uint64 `json:"count"`
}

// RouterUpstream counts a router's failed requests to replicas of a host.
type RouterUpstream struct {
	Host  string `json:"host"`
	Kind  string `json:"kind"`
	Count uint64 `json:"count"`
}

// RouterCert is when the certificate a router serves for a host expires.
type RouterCert struct {
	Host     string    `json:"host"`
	NotAfter time.Time `json:"not_after"`
}

func validRouterHost(h string) error {
	if h == UnknownHost {
		return nil
	}
	return spec.ValidateHost(h)
}

// Validate checks a router report's form: its size, hosts, status codes and
// error kinds, and that no series is there twice, which would make the
// server's /metrics unreadable to Prometheus. Whether the hosts are routed
// is the server's check.
func (r RouterReport) Validate() error {
	if n := len(r.Requests) + len(r.Upstream) + len(r.Certs) + len(r.Refused); n > maxRouterSeries {
		return fmt.Errorf("%w: %d series, more than %d", ErrInvalid, n, maxRouterSeries)
	}
	type pair struct {
		host, v string
	}
	seen := make(map[pair]bool, len(r.Requests)+len(r.Upstream))
	for _, x := range r.Requests {
		if err := validRouterHost(x.Host); err != nil {
			return fmt.Errorf("%w: requests: %v", ErrInvalid, err)
		}
		if x.Code < 100 || x.Code > 599 {
			return fmt.Errorf("%w: requests for %s: status code %d", ErrInvalid, x.Host, x.Code)
		}
		k := pair{x.Host, strconv.Itoa(x.Code)}
		if seen[k] {
			return fmt.Errorf("%w: requests for %s with status %d are there twice", ErrInvalid, x.Host, x.Code)
		}
		seen[k] = true
	}
	for _, x := range r.Upstream {
		if err := validRouterHost(x.Host); err != nil {
			return fmt.Errorf("%w: upstream errors: %v", ErrInvalid, err)
		}
		if !slices.Contains(routerErrorKinds, x.Kind) {
			return fmt.Errorf("%w: upstream errors for %s: kind %q", ErrInvalid, x.Host, truncate(x.Kind))
		}
		k := pair{x.Host, "kind:" + x.Kind}
		if seen[k] {
			return fmt.Errorf("%w: %s errors for %s are there twice", ErrInvalid, x.Kind, x.Host)
		}
		seen[k] = true
	}
	certs := make(map[string]bool, len(r.Certs))
	for _, x := range r.Certs {
		if err := spec.ValidateHost(x.Host); err != nil {
			return fmt.Errorf("%w: certificates: %v", ErrInvalid, err)
		}
		if x.NotAfter.IsZero() {
			return fmt.Errorf("%w: certificate for %s has no expiry", ErrInvalid, x.Host)
		}
		if certs[x.Host] {
			return fmt.Errorf("%w: the certificate for %s is there twice", ErrInvalid, x.Host)
		}
		certs[x.Host] = true
	}
	refused := make(map[string]bool, len(r.Refused))
	for _, x := range r.Refused {
		if !slices.Contains(routerRefusedReasons, x.Reason) {
			return fmt.Errorf("%w: refused connections: reason %q", ErrInvalid, truncate(x.Reason))
		}
		if refused[x.Reason] {
			return fmt.Errorf("%w: connections refused for %s are there twice", ErrInvalid, x.Reason)
		}
		refused[x.Reason] = true
	}
	return nil
}

// Routed returns the report with only the hosts in routed, and the unknown
// host's request counts: what a worker reports for a host the server does
// not route is dropped, so a worker cannot add series of its choosing. The
// refused connections, which have no host, stay.
func (r RouterReport) Routed(routed map[string]bool) RouterReport {
	out := RouterReport{Refused: r.Refused}
	for _, x := range r.Requests {
		if routed[x.Host] || x.Host == UnknownHost {
			out.Requests = append(out.Requests, x)
		}
	}
	for _, x := range r.Upstream {
		if routed[x.Host] {
			out.Upstream = append(out.Upstream, x)
		}
	}
	for _, x := range r.Certs {
		if routed[x.Host] {
			out.Certs = append(out.Certs, x)
		}
	}
	return out
}

// ReportRouter posts the node's router counters. A server older than 2.2
// answers ErrNoRoute.
func (c *Client) ReportRouter(ctx context.Context, nodeID string, r RouterReport) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxRouterReportBytes {
		return fmt.Errorf("router report is %d bytes, more than the %d the server takes", len(b), MaxRouterReportBytes)
	}
	return c.do(ctx, http.MethodPost, RouterMetricsPath(nodeID), b, nil)
}

// RouterReports keeps the last router report of each worker, on the server,
// for /metrics. It is safe for concurrent use; a nil one keeps nothing.
type RouterReports struct {
	mu sync.Mutex
	m  map[string]routerEntry
}

type routerEntry struct {
	r  RouterReport
	at time.Time
}

// NewRouterReports returns an empty holder.
func NewRouterReports() *RouterReports { return &RouterReports{m: map[string]routerEntry{}} }

// Put records node's report, received at at.
func (rs *RouterReports) Put(node string, r RouterReport, at time.Time) {
	if rs == nil {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.m[node] = routerEntry{r, at}
}

// Fresh returns each node's last report received after now minus maxAge,
// and forgets the older ones: a worker that stopped reporting, because it is
// down or was removed, drops out of the metrics.
func (rs *RouterReports) Fresh(now time.Time, maxAge time.Duration) map[string]RouterReport {
	if rs == nil {
		return nil
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make(map[string]RouterReport, len(rs.m))
	for node, e := range rs.m {
		if now.Sub(e.at) > maxAge {
			delete(rs.m, node)
			continue
		}
		out[node] = e.r
	}
	return out
}

// CountOf turns a counter's float value into a report's count.
func CountOf(v float64) uint64 {
	if v <= 0 || math.IsNaN(v) {
		return 0
	}
	if v >= math.MaxUint64 {
		return math.MaxUint64
	}
	return uint64(v)
}
