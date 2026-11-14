package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Upstream limits. The routers reach the replicas over the nodes' private
// network, so a dial that takes longer than upstreamDialTimeout is treated as
// failed and the next replica is tried.
const (
	upstreamDialTimeout     = 2 * time.Second
	upstreamResponseTimeout = 60 * time.Second
	upstreamIdleTimeout     = 90 * time.Second
	upstreamMaxIdlePerHost  = 64
	maxResponseHeaderBytes  = 64 << 10
	// downFor is how long an endpoint whose dial failed is skipped. After it,
	// the endpoint is tried again in its turn.
	downFor = 3 * time.Second
)

// Table is the routing table: for each host, the address:port of every
// replica that takes its traffic now. A host with no endpoints is known, and
// answered with 503.
type Table map[string][]string

// endpoint is one replica's address, with its own connection pool and the
// time until which it is skipped after a failed dial. An endpoint is shared
// by every host that routes to it.
type endpoint struct {
	addr      string
	tr        *http.Transport
	downUntil atomic.Int64 // unix nanoseconds
}

func newEndpoint(addr string) *endpoint {
	d := &net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second}
	return &endpoint{addr: addr, tr: &http.Transport{
		// Never an HTTP proxy from the environment: replicas are on the
		// private network.
		Proxy:                  nil,
		DialContext:            d.DialContext,
		MaxIdleConnsPerHost:    upstreamMaxIdlePerHost,
		IdleConnTimeout:        upstreamIdleTimeout,
		ResponseHeaderTimeout:  upstreamResponseTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		// The client's Accept-Encoding goes to the app, and its reply comes
		// back as it is.
		DisableCompression: true,
	}}
}

// pool is one host's endpoints, in table order, and its round-robin counter.
type pool struct {
	endpoints []*endpoint
	next      atomic.Uint64
}

// order returns the endpoints to try for one request: round-robin from the
// next one, those that are up first, then those marked down, which are tried
// only when every endpoint that is up has failed.
func (p *pool) order(now time.Time) []*endpoint {
	n := len(p.endpoints)
	if n == 0 {
		return nil
	}
	start := int(p.next.Add(1)-1) % n
	up := make([]*endpoint, 0, n)
	var down []*endpoint
	for i := range n {
		e := p.endpoints[(start+i)%n]
		if e.downUntil.Load() > now.UnixNano() {
			down = append(down, e)
		} else {
			up = append(up, e)
		}
	}
	return append(up, down...)
}

// proxy holds the table and routes requests by host.
type proxy struct {
	log     *slog.Logger
	metrics *Metrics
	now     func() time.Time
	rp      *httputil.ReverseProxy

	mu        sync.RWMutex
	pools     map[string]*pool
	endpoints map[string]*endpoint
}

func newProxy(log *slog.Logger, m *Metrics) *proxy {
	p := &proxy{log: log, metrics: m, now: time.Now, pools: map[string]*pool{}, endpoints: map[string]*endpoint{}}
	p.rp = &httputil.ReverseProxy{
		Rewrite:      rewrite,
		Transport:    balancer{p},
		ErrorHandler: p.upstreamFailed,
		ErrorLog:     slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return p
}

// rewrite sets the outbound request. Rewrite mode has already removed the
// client's X-Forwarded-For, -Host and -Proto; SetXForwarded sets them again
// from the connection, so a client cannot choose them. Forwarded and
// X-Real-Ip, which ReverseProxy passes on, are removed for the same reason.
// The Host header stays the client's. The balancer fills in the endpoint.
func rewrite(pr *httputil.ProxyRequest) {
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = pr.In.Host
	pr.Out.Header.Del("Forwarded")
	pr.Out.Header.Del("X-Real-Ip")
	pr.SetXForwarded()
}

// setTable replaces the routing table. Endpoints that stay keep their
// connections and their down marks; endpoints that leave close their idle
// connections. A request in flight to one finishes.
func (p *proxy) setTable(t Table) {
	pools := make(map[string]*pool, len(t))
	eps := make(map[string]*endpoint)
	p.mu.Lock()
	for host, addrs := range t {
		pl := &pool{}
		if old := p.pools[host]; old != nil {
			pl.next.Store(old.next.Load())
		}
		for _, a := range addrs {
			e := eps[a]
			if e == nil {
				if e = p.endpoints[a]; e == nil {
					e = newEndpoint(a)
				}
				eps[a] = e
			}
			pl.endpoints = append(pl.endpoints, e)
		}
		pools[host] = pl
	}
	var gone []*endpoint
	for a, e := range p.endpoints {
		if eps[a] == nil {
			gone = append(gone, e)
		}
	}
	p.pools, p.endpoints = pools, eps
	p.mu.Unlock()
	for _, e := range gone {
		e.tr.CloseIdleConnections()
	}
}

// pool returns the host's endpoints, or nil for a host not in the table.
func (p *proxy) pool(host string) *pool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pools[host]
}

// known reports whether host is in the table.
func (p *proxy) known(host string) bool { return p.pool(host) != nil }

// hosts returns the hosts in the table, sorted.
func (p *proxy) hosts() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.pools))
	for h := range p.pools {
		out = append(out, h)
	}
	slices.Sort(out)
	return out
}

type ctxKey struct{}

type reqInfo struct {
	host string
	pool *pool
}

// ServeHTTP is the HTTPS handler. A host the table does not have gets a
// plain 404, which names no other host; a known host with no Ready replica
// gets 503. Over TLS, the Host header must name the host the client asked
// for in SNI, or the reply is 421.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if r.TLS != nil && r.TLS.ServerName != "" && !strings.EqualFold(r.TLS.ServerName, host) {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	pl := p.pool(host)
	switch {
	case pl == nil:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case len(pl.endpoints) == 0:
		http.Error(w, "no replica is ready", http.StatusServiceUnavailable)
		return
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, reqInfo{host: host, pool: pl})
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// upstreamFailed answers a request that no replica took.
func (p *proxy) upstreamFailed(w http.ResponseWriter, r *http.Request, err error) {
	info, _ := r.Context().Value(ctxKey{}).(reqInfo)
	if r.Context().Err() == nil {
		// A request the client gave up on is not the replica's failure.
		p.metrics.upstreamError(info.host, "proxy")
	}
	p.log.Debug("upstream request failed", "host", info.host, "err", err)
	http.Error(w, "bad gateway", http.StatusBadGateway)
}

// balancer is the proxy's RoundTripper. It sends the request to the host's
// endpoints in round-robin order. An endpoint whose dial fails is marked down
// for downFor and the next one is tried, as long as nothing of the request
// body has been read: a request whose body has gone to one replica is not
// sent to another.
type balancer struct{ p *proxy }

func (b balancer) RoundTrip(req *http.Request) (*http.Response, error) {
	info, ok := req.Context().Value(ctxKey{}).(reqInfo)
	if !ok {
		return nil, errors.New("router: request without a route")
	}
	var guard *bodyGuard
	if req.Body != nil && req.Body != http.NoBody {
		guard = &bodyGuard{rc: req.Body}
	}
	var lastErr error
	for _, e := range info.pool.order(b.p.now()) {
		out := req.Clone(req.Context())
		out.URL.Host = e.addr
		if guard != nil {
			out.Body = guard
		}
		resp, err := e.tr.RoundTrip(out)
		if err == nil {
			e.downUntil.Store(0)
			return resp, nil
		}
		lastErr = err
		if !isDialError(err) {
			return nil, err
		}
		e.downUntil.Store(b.p.now().Add(downFor).UnixNano())
		b.p.metrics.upstreamError(info.host, "dial")
		b.p.log.Debug("replica unreachable; trying the next", "host", info.host, "endpoint", e.addr, "err", err)
		if guard != nil && guard.read.Load() {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("router: no endpoints")
	}
	return nil, lastErr
}

// isDialError reports whether err is a failure to connect, so the request
// never reached the replica.
func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// bodyGuard wraps a request body so it can be offered to a second replica
// after a failed dial. It records whether anything read it, and ignores
// Close, which the transport calls on every failure; the server closes the
// real body when the handler returns.
type bodyGuard struct {
	rc   io.ReadCloser
	read atomic.Bool
}

func (g *bodyGuard) Read(p []byte) (int, error) {
	g.read.Store(true)
	return g.rc.Read(p)
}

func (g *bodyGuard) Close() error { return nil }

// requestHost is the request's host without a port, in lowercase, without a
// trailing dot.
func requestHost(r *http.Request) string {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}
