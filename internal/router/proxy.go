package router

import (
	"cmp"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strconv"
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
	// the endpoint is tried again in its turn. Each failure in a row after
	// that doubles the time, up to downForMost; a request that gets through
	// ends the run, and so does the endpoint leaving the table.
	downFor     = 3 * time.Second
	downForMost = 30 * time.Second

	// upstreamUserTimeout is TCP_USER_TIMEOUT on Linux: how long data the
	// replica's machine has not acknowledged may wait before the connection
	// fails (see setUserTimeout).
	upstreamUserTimeout = 5 * time.Second
)

// Table is the routing table: the routes of each host. A host in the table
// is known, whatever its routes: it has a certificate, and a request for a
// path none of its routes takes gets 404.
type Table map[string][]Route

// Route is one route of a host: a path prefix, and the address:port of
// every replica that takes its traffic now. Path "" is the whole host; any
// other path is a prefix as spec.ValidatePath allows, such as "/api", which
// takes "/api" and "/api/..." but not "/apix". A route with no endpoints is
// answered with 503.
type Route struct {
	Path      string
	Endpoints []string
}

// endpoint is one replica's address, with its own connection pool and the
// time until which it is skipped after a failed dial. An endpoint is shared
// by every host that routes to it.
type endpoint struct {
	addr      string
	tr        *http.Transport
	downUntil atomic.Int64 // unix nanoseconds
	// fails is the failures in a row, each counted once per down mark.
	// mu orders its changes with downUntil's.
	fails atomic.Int32
	mu    sync.Mutex
}

// failed marks the endpoint down after a failure at now, and returns for
// how long: downFor after the first failure in a row, doubling with each
// that follows, up to downForMost. While the server's machine was gone in
// the third soak, the workers' routers dialled its dead replicas every 3 s,
// and each dial held a live request for the 2 s dial timeout. A failure
// while the endpoint is already marked down, as of a request that tried it
// last because every other endpoint failed too, or of one that raced
// another to it, does not lengthen the mark and returns 0.
func (e *endpoint) failed(now time.Time) time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.downUntil.Load() > now.UnixNano() {
		return 0
	}
	n := e.fails.Add(1)
	d := downFor
	for i := int32(1); i < n && d < downForMost; i++ {
		d *= 2
	}
	d = min(d, downForMost)
	e.downUntil.Store(now.Add(d).UnixNano())
	return d
}

// ok ends a run of failures: the endpoint answered.
func (e *endpoint) ok() {
	if e.fails.Load() == 0 && e.downUntil.Load() == 0 {
		return
	}
	e.mu.Lock()
	e.fails.Store(0)
	e.downUntil.Store(0)
	e.mu.Unlock()
}

func newEndpoint(addr string) *endpoint {
	d := &net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second, Control: setUserTimeout}
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

// pool is one route's endpoints, in table order, and its round-robin
// counter.
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

// hostRoutes is one host's routes, longest path first, so that the first
// one that matches a request is its longest prefix; the whole host's route,
// if there is one, is last.
type hostRoutes struct {
	routes []*route
	// paths is set when a route has a path. Requests then go through
	// routingPath, and some are refused.
	paths bool
}

type route struct {
	path string
	pool *pool
}

// find returns the route with path p, or nil.
func (h *hostRoutes) find(p string) *route {
	if h == nil {
		return nil
	}
	for _, r := range h.routes {
		if r.path == p {
			return r
		}
	}
	return nil
}

// match returns the route that takes a request for u: the one whose path is
// the longest prefix of the request's path on a segment boundary, or else
// the whole host's. It is nil when no route takes it. A host without path
// routes sends every request to its whole host route, as before paths
// existed. For a host with path routes, ok is false when routingPath refuses
// the path.
func (h *hostRoutes) match(u *url.URL) (rt *route, ok bool) {
	if !h.paths {
		if len(h.routes) == 0 {
			return nil, true
		}
		return h.routes[0], true
	}
	p, ok := routingPath(u)
	if !ok {
		return nil, false
	}
	for _, r := range h.routes {
		if r.path == "" || underPrefix(p, r.path) {
			return r, true
		}
	}
	return nil, true
}

// underPrefix reports whether the clean path p is prefix or below it: the
// prefix ends at a segment boundary of p. "/api" has "/api" and "/api/x"
// under it, and not "/apix".
func underPrefix(p, prefix string) bool {
	return strings.HasPrefix(p, prefix) && (len(p) == len(prefix) || p[len(prefix)] == '/')
}

// routingPath returns the path that a request for u is matched on, on a
// host with path routes: the decoded path, with empty segments and a
// trailing slash cleaned away, so "//api/x/" is matched as "/api/x". A path
// that does not start with "/", such as the "*" of OPTIONS *, comes back as
// "", which only the whole host's route takes.
//
// ok is false for a path that the app behind the route, or a proxy or
// framework in front of it, could read as being under another prefix than
// the one the router would choose. The router refuses such a request with
// 400 rather than guess:
//
//   - an encoded slash or backslash (%2F, %5C). Decoded, it splits a segment
//     in two; left encoded, it does not. Apps differ in which they do, so
//     "/api%2Fx" is under /api to some and not to others;
//   - a backslash, which some servers read as a slash;
//   - a "." or ".." segment, written as it is or encoded (%2E), and also
//     when ";" parameters follow it ("..;"), which servlet containers
//     strip before they resolve the dots. "/api/../admin" is /admin, but an
//     app that does not resolve dots itself would see a path under /api, or
//     the reverse: "/admin/../api/x" would reach the /api app, which could
//     serve its own /admin for it;
//   - a control character, NUL included.
//
// Browsers and curl remove dot segments before they send a request, and
// encode neither slashes nor backslashes in paths, so these are requests
// that a person or a script crafted. A host without path routes passes them
// to its app as before.
func routingPath(u *url.URL) (string, bool) {
	// RawPath is set only when the client's escaping differs from Go's
	// own, which never encodes a slash; an encoded backslash is a
	// backslash in Path.
	if raw := strings.ToLower(u.RawPath); strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") {
		return "", false
	}
	p := u.Path
	for i := 0; i < len(p); i++ {
		if c := p[i]; c < 0x20 || c == 0x7f || c == '\\' {
			return "", false
		}
	}
	if !strings.HasPrefix(p, "/") {
		return "", true
	}
	for seg := range strings.SplitSeq(p[1:], "/") {
		name, _, _ := strings.Cut(seg, ";")
		if name == "." || name == ".." {
			return "", false
		}
	}
	return path.Clean(p), true
}

// proxy holds the table and routes requests by host and path.
type proxy struct {
	log     *slog.Logger
	metrics *Metrics
	now     func() time.Time
	rp      *httputil.ReverseProxy
	// httpsPort is the port of the https listener, which clients connect
	// to, and upgradeIdle the idle limit of upgraded connections. New sets
	// them before anything is served.
	httpsPort   int
	upgradeIdle time.Duration

	mu        sync.RWMutex
	table     map[string]*hostRoutes
	endpoints map[string]*endpoint
}

func newProxy(log *slog.Logger, m *Metrics) *proxy {
	p := &proxy{log: log, metrics: m, now: time.Now, table: map[string]*hostRoutes{}, endpoints: map[string]*endpoint{}}
	p.rp = &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      balancer{p},
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.upstreamFailed,
		ErrorLog:       slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return p
}

// clientHeaders are the request headers, other than X-Forwarded-*, in which
// a proxy in front of an app names the client or what the client asked for.
// A client can send any of them, so the router removes them all, and every
// header that starts with clientHeaderPrefix, before it sets its own. This is
// the one list of them.
var clientHeaders = []string{
	"forwarded", "x-real-ip", "true-client-ip", "x-client-ip", "x-cluster-client-ip", "cf-connecting-ip",
}

// clientHeaderPrefix starts X-Forwarded-For, -Host, -Proto, -Port, -Prefix,
// -Ssl, -Server and the rest of that family.
const clientHeaderPrefix = "x-forwarded-"

// isClientHeader reports whether name is one of the headers the router
// removes. Names are compared without case and with "_" read as "-": a
// CGI-style server (WSGI, PHP) turns both into one variable, so a client's
// X_Forwarded_For would reach the app as HTTP_X_FORWARDED_FOR.
func isClientHeader(name string) bool {
	n := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return strings.HasPrefix(n, clientHeaderPrefix) || slices.Contains(clientHeaders, n)
}

// authority is what the router sends as Host and X-Forwarded-Host for host,
// a table host: the host, with the https port when it is not 443. It is the
// authority of the router's own redirects to https, so the URLs an app builds
// from Host match the ones the router sends clients to.
func (p *proxy) authority(host string) string {
	if p.httpsPort == 443 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(p.httpsPort))
}

// rewrite sets the outbound request. Every header a client could use to
// name another client address, host, scheme or port is removed (see
// clientHeaders), and the router sets X-Forwarded-For to the address the
// connection came from, X-Forwarded-Proto to https, X-Forwarded-Port to the
// https port, and Host and X-Forwarded-Host to the table's host (see
// authority), not the client's spelling of it: its case, a trailing dot and
// any port it wrote are gone. The balancer fills in the endpoint.
func (p *proxy) rewrite(pr *httputil.ProxyRequest) {
	info, _ := pr.In.Context().Value(ctxKey{}).(reqInfo)
	authority := p.authority(info.host)
	pr.Out.URL.Scheme = "http"
	pr.Out.URL.Host = authority
	pr.Out.Host = authority
	for name := range pr.Out.Header {
		if isClientHeader(name) {
			delete(pr.Out.Header, name)
		}
	}
	dropH2C(pr.Out.Header)
	pr.SetXForwarded()
	pr.Out.Header.Set("X-Forwarded-Host", authority)
	pr.Out.Header.Set("X-Forwarded-Port", strconv.Itoa(p.httpsPort))
}

// dropH2C takes the h2c token out of an outbound request's Upgrade header,
// and removes the header, with its Connection: Upgrade, when no other
// protocol is left; HTTP2-Settings goes too. h2c is HTTP/2 over cleartext
// TCP (RFC 9113, 3.1), which has no place on a TLS connection, and a replica
// that switched to it would speak HTTP/2 with the client directly, past the
// router's host check and header rewriting, for the rest of the connection.
// RFC 9110 (7.8) lets a server ignore Upgrade, so the request goes on as
// HTTP/1.1, as it would to a server without h2c; a client is not refused for
// asking. A replica that answers 101 for a protocol the request no longer
// names gets the client a 502 from ReverseProxy. Other upgrades, such as
// WebSocket, pass.
func dropH2C(h http.Header) {
	h.Del("Http2-Settings")
	vals := h.Values("Upgrade")
	if len(vals) == 0 {
		return
	}
	var keep []string
	for _, v := range vals {
		for tok := range strings.SplitSeq(v, ",") {
			tok = strings.TrimSpace(tok)
			name, _, _ := strings.Cut(tok, "/")
			if tok != "" && !strings.EqualFold(name, "h2c") {
				keep = append(keep, tok)
			}
		}
	}
	if len(keep) == 0 {
		h.Del("Upgrade")
		h.Del("Connection")
		return
	}
	h.Set("Upgrade", strings.Join(keep, ", "))
}

// setTable replaces the routing table. Endpoints that stay keep their
// connections, their down marks and their failures in a row, and a route
// that stays keeps its place in the round-robin; endpoints that leave close
// their idle connections, and come back with no failures. A
// request in flight to one finishes. A path listed twice for one host keeps
// its first entry; the table's validation refuses that before it gets here.
func (p *proxy) setTable(t Table) {
	hosts := make(map[string]*hostRoutes, len(t))
	eps := make(map[string]*endpoint)
	p.mu.Lock()
	for host, routes := range t {
		hr := &hostRoutes{}
		old := p.table[host]
		for _, r := range routes {
			if hr.find(r.Path) != nil {
				continue
			}
			pl := &pool{}
			if o := old.find(r.Path); o != nil {
				pl.next.Store(o.pool.next.Load())
			}
			for _, a := range r.Endpoints {
				e := eps[a]
				if e == nil {
					if e = p.endpoints[a]; e == nil {
						e = newEndpoint(a)
					}
					eps[a] = e
				}
				pl.endpoints = append(pl.endpoints, e)
			}
			hr.routes = append(hr.routes, &route{path: r.Path, pool: pl})
			if r.Path != "" {
				hr.paths = true
			}
		}
		// Longest path first, so the first match is the longest prefix;
		// the whole host's "" sorts last.
		slices.SortStableFunc(hr.routes, func(a, b *route) int { return cmp.Compare(len(b.path), len(a.path)) })
		hosts[host] = hr
	}
	var gone []*endpoint
	for a, e := range p.endpoints {
		if eps[a] == nil {
			gone = append(gone, e)
		}
	}
	p.table, p.endpoints = hosts, eps
	p.mu.Unlock()
	for _, e := range gone {
		e.tr.CloseIdleConnections()
	}
}

// lookup returns the host's routes, or nil for a host not in the table.
func (p *proxy) lookup(host string) *hostRoutes {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.table[host]
}

// known reports whether host is in the table.
func (p *proxy) known(host string) bool { return p.lookup(host) != nil }

// hosts returns the hosts in the table, sorted.
func (p *proxy) hosts() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.table))
	for h := range p.table {
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
// plain 404, which names no other host. Over TLS, the Host header must name
// the host the client asked for in SNI, or the reply is 421. On a known host
// the request goes to the route that match chooses: a path that no route
// takes gets 404, a path that routingPath refuses gets 400, and a route with
// no Ready replica gets 503. The request's path goes to the replica as the
// client sent it: the router matches on it and does not rewrite it.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if r.TLS != nil && r.TLS.ServerName != "" && !strings.EqualFold(r.TLS.ServerName, host) {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	hr := p.lookup(host)
	if hr == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rt, ok := hr.match(r.URL)
	switch {
	case !ok:
		http.Error(w, "bad request: the path has an encoded slash or backslash, a backslash, a dot segment or a control character", http.StatusBadRequest)
		return
	case rt == nil:
		http.Error(w, "not found", http.StatusNotFound)
		return
	case len(rt.pool.endpoints) == 0:
		http.Error(w, "no replica is ready", http.StatusServiceUnavailable)
		return
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, reqInfo{host: host, pool: rt.pool})
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
// (endpoint.failed: downFor, doubling while its dials keep failing, up to
// downForMost) and the next one is tried, as long as nothing of the request
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
			e.ok()
			return resp, nil
		}
		lastErr = err
		if !isDialError(err) {
			// A connection that failed before any reply, as one to a
			// machine that lost power does once TCP_USER_TIMEOUT ends it,
			// is tried on the next replica too, for a request that can be
			// sent twice: a method that is safe to repeat, and no body. This
			// is nginx's proxy_next_upstream default (error, for
			// idempotent methods). Other failures are a 502.
			if !retryable(req, err) || guard != nil {
				return nil, err
			}
			e.failed(b.p.now())
			b.p.metrics.upstreamError(info.host, "connection")
			b.p.log.Debug("replica's connection failed before it replied; trying the next", "host", info.host, "endpoint", e.addr, "err", err)
			continue
		}
		skip := e.failed(b.p.now())
		b.p.metrics.upstreamError(info.host, "dial")
		b.p.log.Debug("replica unreachable; trying the next", "host", info.host, "endpoint", e.addr, "skipped_for", skip, "err", err)
		if guard != nil && guard.read.Load() {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("router: no endpoints")
	}
	return nil, lastErr
}

// retryable reports whether a request that failed with err, after its
// connection was made, may go to another replica: the client is still
// waiting, the method is safe to repeat (RFC 9110, 9.2.2), and the failure
// was the connection's (a reset, a timeout of TCP_USER_TIMEOUT, an EOF
// before the reply), not the response timeout of a replica that is up.
func retryable(req *http.Request, err error) bool {
	if req.Context().Err() != nil {
		return false
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	var op *net.OpError
	return errors.As(err, &op) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
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
