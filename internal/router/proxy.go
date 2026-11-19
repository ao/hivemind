package router

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ao/hivemind/internal/spec"
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

	// staleTable is how old the table must be for a request that no
	// replica of its route took to read it again (newerTable). A table read
	// less than this ago is taken as current, and the request gets 502 at
	// once.
	staleTable = 500 * time.Millisecond
)

// Table is the routing table: the routes of each host. A host in the table
// is known, whatever its routes: it has a certificate, and a request for a
// path none of its routes takes gets 404.
type Table map[string][]Route

// Route is one route of a host: a path prefix, the protocol its replicas
// speak, and the address:port of every replica that takes its traffic now.
// Path "" is the whole host; any other path is a prefix as spec.ValidatePath
// allows, such as "/api", which takes "/api" and "/api/..." but not "/apix".
// Protocol "" is HTTP/1.1, and ProtocolH2C is HTTP/2 without TLS; a route
// with any other protocol gets no endpoints. A route with no endpoints is
// answered with 503.
type Route struct {
	Path      string
	Protocol  string
	Endpoints []string
}

// ProtocolH2C is the protocol of a route whose replicas speak HTTP/2 over
// cleartext TCP with prior knowledge (RFC 9113, 3.3), as a gRPC server does.
const ProtocolH2C = spec.RouteH2C

// epKey is what an endpoint is known by: its address, and whether it speaks
// h2c. Two routes can name one container port with different protocols, and
// each gets its own endpoint, with its own connections and down mark.
type epKey struct {
	addr string
	h2c  bool
}

// endpoint is one replica's address, with its own connection pool and the
// time until which it is skipped after a failed dial. An endpoint is shared
// by every host that routes to it with the same protocol.
type endpoint struct {
	addr      string
	h2c       bool
	tr        *http.Transport
	downUntil atomic.Int64 // unix nanoseconds
	// fails is the failures in a row, each counted once per down mark.
	// mu orders its changes with downUntil's.
	fails atomic.Int32
	mu    sync.Mutex
	// fresh is tr with no connections kept, made when a request first
	// needs a new connection (freshTransport); under mu.
	fresh *http.Transport
}

// freshTransport returns a transport like tr that opens a new connection
// for each request and keeps none, for a request whose connection to
// another replica failed: a pooled connection to this one may be as dead,
// and would take TCP_USER_TIMEOUT to fail, where a new one fails at the dial
// timeout.
func (e *endpoint) freshTransport() *http.Transport {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fresh == nil {
		e.fresh = e.tr.Clone()
		e.fresh.DisableKeepAlives = true
	}
	return e.fresh
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

// newEndpoint returns the endpoint for addr. Its transport speaks HTTP/1.1,
// or, with h2c, HTTP/2 with prior knowledge and nothing else: an h2c route's
// replicas, such as gRPC servers, may not speak HTTP/1.1 at all. Both dial
// alike, with the dial timeout, keepalives and TCP_USER_TIMEOUT, and wait
// responseTimeout for a reply's headers once the whole request is sent.
// Over HTTP/2 one connection carries many requests at once; the transport
// opens another when the replica's stream limit is reached.
func newEndpoint(addr string, h2c bool, responseTimeout time.Duration) *endpoint {
	d := &net.Dialer{Timeout: upstreamDialTimeout, KeepAlive: 30 * time.Second, Control: setUserTimeout}
	tr := &http.Transport{
		// Never an HTTP proxy from the environment: replicas are on the
		// private network.
		Proxy:                  nil,
		DialContext:            d.DialContext,
		MaxIdleConnsPerHost:    upstreamMaxIdlePerHost,
		IdleConnTimeout:        upstreamIdleTimeout,
		ResponseHeaderTimeout:  responseTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		// The client's Accept-Encoding goes to the app, and its reply comes
		// back as it is.
		DisableCompression: true,
	}
	if h2c {
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetUnencryptedHTTP2(true)
	}
	return &endpoint{addr: addr, h2c: h2c, tr: tr}
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
	h2c  bool
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
// existed. For a host with path routes, bad is routingPath's reason when it
// refuses the path.
func (h *hostRoutes) match(u *url.URL) (rt *route, bad string) {
	if !h.paths {
		if len(h.routes) == 0 {
			return nil, ""
		}
		return h.routes[0], ""
	}
	p, bad := routingPath(u)
	if bad != "" {
		return nil, bad
	}
	for _, r := range h.routes {
		if r.path == "" || underPrefix(p, r.path) {
			return r, ""
		}
	}
	return nil, ""
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
// bad is the first reason found to refuse a path that the app behind the
// route, or a proxy or framework in front of it, could read as being under
// another prefix than the one the router would choose. The router refuses
// such a request with 400, and that reason in the body, rather than guess:
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
func routingPath(u *url.URL) (p, bad string) {
	// RawPath is set only when the client's escaping differs from Go's
	// own, which never encodes a slash; an encoded backslash is a
	// backslash in Path.
	switch raw := strings.ToLower(u.RawPath); {
	case strings.Contains(raw, "%2f"):
		return "", "an encoded slash (%2F)"
	case strings.Contains(raw, "%5c"):
		return "", "an encoded backslash (%5C)"
	}
	p = u.Path
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c < 0x20 || c == 0x7f:
			return "", "a control character"
		case c == '\\':
			return "", `a backslash (\ or %5C)`
		}
	}
	if !strings.HasPrefix(p, "/") {
		return "", ""
	}
	for seg := range strings.SplitSeq(p[1:], "/") {
		name, _, _ := strings.Cut(seg, ";")
		if name == "." || name == ".." {
			return "", "a dot segment (. or .., or %2E)"
		}
	}
	return path.Clean(p), ""
}

// proxy holds the table and routes requests by host and path.
type proxy struct {
	log     *slog.Logger
	metrics *Metrics
	now     func() time.Time
	// rp proxies the requests of http routes, and rpH2C those of h2c
	// routes, which it flushes to the client after every write.
	rp, rpH2C *httputil.ReverseProxy
	// httpsPort is the port of the https listener, which clients connect
	// to, and upgradeIdle the idle limit of upgraded connections and of
	// requests on h2c routes. New sets them before anything is served.
	httpsPort   int
	upgradeIdle time.Duration
	// responseTimeout is how long a replica has to send a reply's headers
	// once the whole request is sent: upstreamResponseTimeout, but for
	// tests. Endpoints take it when they are made.
	responseTimeout time.Duration
	// dial, if set, dials the replicas in place of the endpoints' own
	// dialer. Tests set it, before the first table.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// userTimeout is upstreamUserTimeout, but for tests: a try whose
	// pooled connection failed this long after the request was written on
	// it is taken for a connection that timed out (errMachineTimedOut).
	userTimeout time.Duration
	// refresh and awaitWhole are the router's (Router.refresh and
	// Router.awaitWhole), for a request whose route has no replica that
	// takes it (newerTable). Either may be nil. New sets them before
	// anything is served.
	refresh    func(ctx context.Context)
	awaitWhole func(ctx context.Context) bool
	// staleAfter is staleTable, but for tests.
	staleAfter time.Duration
	// tableAt is when the table was last set, in unix nanoseconds of now.
	tableAt atomic.Int64

	mu        sync.RWMutex
	table     map[string]*hostRoutes
	endpoints map[epKey]*endpoint
}

func newProxy(log *slog.Logger, m *Metrics) *proxy {
	p := &proxy{log: log, metrics: m, now: time.Now, responseTimeout: upstreamResponseTimeout, userTimeout: upstreamUserTimeout,
		staleAfter: staleTable, table: map[string]*hostRoutes{}, endpoints: map[epKey]*endpoint{}}
	p.rp = p.reverseProxy(0)
	// A negative FlushInterval flushes after every write. ReverseProxy
	// does that for a reply without a Content-Length anyway, which a gRPC
	// reply is; for h2c it is set whatever the reply, so a stream never
	// waits in the router for a buffer to fill.
	p.rpH2C = p.reverseProxy(-1)
	return p
}

func (p *proxy) reverseProxy(flush time.Duration) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        p.rewrite,
		Transport:      balancer{p},
		FlushInterval:  flush,
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.upstreamFailed,
		ErrorLog:       slog.NewLogLogger(p.log.Handler(), slog.LevelWarn),
	}
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
//
// On an h2c route the request goes to the replica over HTTP/2, which has no
// Upgrade (RFC 9113, 8.2.2), so Upgrade and Connection are removed too, and
// an upgrade such as a WebSocket reaches the replica as a plain request, as
// a server may ignore an upgrade (RFC 9110, 7.8).
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
	if info.h2c {
		pr.Out.Header.Del("Upgrade")
		pr.Out.Header.Del("Connection")
	}
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
// its first entry; the table's validation refuses that before it gets here,
// and a protocol other than HTTP/1.1 and h2c, which leaves the route without
// endpoints.
func (p *proxy) setTable(t Table) {
	hosts := make(map[string]*hostRoutes, len(t))
	eps := make(map[epKey]*endpoint)
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
			h2c := r.Protocol == ProtocolH2C
			if r.Protocol == "" || h2c {
				for _, a := range r.Endpoints {
					k := epKey{addr: a, h2c: h2c}
					e := eps[k]
					if e == nil {
						if e = p.endpoints[k]; e == nil {
							e = newEndpoint(a, h2c, p.responseTimeout)
							dial := e.tr.DialContext
							if p.dial != nil {
								dial = p.dial
							}
							e.tr.DialContext = p.dialFor(dial)
						}
						eps[k] = e
					}
					pl.endpoints = append(pl.endpoints, e)
				}
			}
			hr.routes = append(hr.routes, &route{path: r.Path, pool: pl, h2c: h2c})
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
	p.tableAt.Store(p.now().UnixNano())
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
	// path is the route's path, and pool its endpoints in the table the
	// request was routed by.
	path string
	pool *pool
	// h2c is set for a request on an h2c route, and stream then holds its
	// idle limit, if it has one.
	h2c    bool
	stream *streamIdle
}

// ServeHTTP is the HTTPS handler. A host the table does not have gets a
// plain 404, which names no other host. Over TLS, the Host header must name
// the host the client asked for in SNI, or the reply is 421. On a known host
// the request goes to the route that match chooses: a path that no route
// takes gets 404, a path that routingPath refuses gets 400, and a route with
// no Ready replica gets 503. The request's path goes to the replica as the
// client sent it: the router matches on it and does not rewrite it. A
// request on an h2c route goes through serveH2C.
//
// On the server's router at its start, a route with no replica may be one
// whose replicas are all on the server, which the table has only once the
// server's own node has reported: the request waits for that table
// (awaitWhole) before it gets 503.
func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := requestHost(r)
	if r.TLS != nil && r.TLS.ServerName != "" && !strings.EqualFold(r.TLS.ServerName, host) {
		http.Error(w, "misdirected request", http.StatusMisdirectedRequest)
		return
	}
	rt, code, msg := p.routeFor(host, r.URL)
	if code == http.StatusServiceUnavailable && p.awaitWhole != nil && p.awaitWhole(r.Context()) && r.Context().Err() == nil {
		rt, code, msg = p.routeFor(host, r.URL)
	}
	if code != 0 {
		http.Error(w, msg, code)
		return
	}
	info := reqInfo{host: host, path: rt.path, pool: rt.pool, h2c: rt.h2c}
	if rt.h2c {
		p.serveH2C(w, r, info)
		return
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, info)))
}

// routeFor returns the route that takes a request for host and u, or the
// status and body of the reply that refuses it.
func (p *proxy) routeFor(host string, u *url.URL) (rt *route, code int, msg string) {
	hr := p.lookup(host)
	if hr == nil {
		return nil, http.StatusNotFound, "not found"
	}
	rt, bad := hr.match(u)
	switch {
	case bad != "":
		return nil, http.StatusBadRequest, "bad request: the path has " + bad
	case rt == nil:
		return nil, http.StatusNotFound, "not found"
	case len(rt.pool.endpoints) == 0:
		return nil, http.StatusServiceUnavailable, "no replica is ready"
	}
	return rt, 0, ""
}

// serveH2C proxies a request on an h2c route to a replica over HTTP/2. The
// request may be a stream either way that lasts as long as the client and
// the replica keep it open, such as a gRPC server or bidirectional stream,
// and it flows as it comes: the client's body goes to the replica as it is
// read, and rpH2C flushes the reply after every write, with its trailers
// (a gRPC call's status) at the end.
//
// The whole-request read limit of the https server (readTimeout), which
// would cut a client that is still sending after 5 minutes, is lifted for
// the request. Instead, a request that carries no data either way, in its
// body or its reply's, for upgradeIdle is ended, as an upgraded connection
// is. The reply's headers still have the response timeout, which counts from
// when the client has sent its whole request: a server stream whose replica
// sends nothing at all, not even its headers, for that long gets 502.
func (p *proxy) serveH2C(w http.ResponseWriter, r *http.Request, info reqInfo) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	body := r.Body
	if p.upgradeIdle > 0 {
		// Go's HTTP/2 transport does not see the context end while it
		// waits for the client to send more of the body, as in a quiet
		// bidirectional stream, so the end of the limit also ends that
		// wait: a read deadline in the past fails the read, and the
		// transport then resets the replica's stream.
		end := func() {
			_ = rc.SetReadDeadline(time.Now().Add(-time.Second))
			cancel()
		}
		s := newStreamIdle(p.upgradeIdle, end, p.log, info.host)
		defer s.stop()
		info.stream = s
		if body != nil && body != http.NoBody {
			body = &touchBody{ReadCloser: body, s: s}
		}
	}
	// A copy of the request: the server closes the body it made.
	r = r.WithContext(context.WithValue(ctx, ctxKey{}, info))
	r.Body = body
	p.rpH2C.ServeHTTP(w, r)
}

// streamIdle ends a request on an h2c route when no data passes either way
// for idle: it calls end, which fails the request's reads and cancels its
// context. That resets the replica's stream, and the client's reply then
// fails, or is 502 if it had not begun.
type streamIdle struct {
	idle time.Duration
	end  func()
	log  *slog.Logger
	host string
	last atomic.Int64 // unix nanoseconds of the last data either way

	mu    sync.Mutex
	timer *time.Timer
	done  bool
}

func newStreamIdle(idle time.Duration, end func(), log *slog.Logger, host string) *streamIdle {
	s := &streamIdle{idle: idle, end: end, log: log, host: host}
	s.touch()
	s.mu.Lock()
	s.timer = time.AfterFunc(idle, s.check)
	s.mu.Unlock()
	return s
}

// touch records data either way now.
func (s *streamIdle) touch() { s.last.Store(time.Now().UnixNano()) }

// check ends the request if it has been idle for idle, and otherwise looks
// again when it would be. It holds mu while it calls end, so end never runs
// after stop has returned, when the request's writer may be reused.
func (s *streamIdle) check() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	if since := time.Since(time.Unix(0, s.last.Load())); since < s.idle {
		s.timer.Reset(s.idle - since)
		return
	}
	s.done = true
	s.log.Debug("ending a request on an h2c route that carried no data either way", "host", s.host, "idle", s.idle)
	s.end()
}

// stop ends the limit, when the request is done.
func (s *streamIdle) stop() {
	s.mu.Lock()
	s.done = true
	s.timer.Stop()
	s.mu.Unlock()
}

// touchBody is a request or reply body that tells s of every read that
// carried data.
type touchBody struct {
	io.ReadCloser
	s *streamIdle
}

func (b *touchBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.s.touch()
	}
	return n, err
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
//
// A connection that times out, as one to a machine that lost power does once
// TCP_USER_TIMEOUT ends it, says the machine is gone, not the replica: every
// endpoint at that address is marked down, and the request skips them. After
// any failed connection the request goes on new connections, whose dial
// timeout bounds a dead machine at 2 s, rather than on pooled ones, each of
// which would take the whole 5 s to fail. In the second scale run, with two
// of a route's dead replicas next to each other in its order, every router
// that stayed up failed one GET at the client's 10 s limit, 5 s on each.
//
// A request that every endpoint of its route failed in one of those ways,
// and that can still go to another, tries the endpoints of the route in a
// newer table that it has not tried, if there is one (newerTable), before it
// gets 502: the replica that would take it may have come into the table
// since the router last read it. In the seventh soak, the server's router,
// whose first table was read before the server's own node had reported,
// answered 502 for up to a second after each start for an app whose replica
// on the server ran, while the app's other replicas were down.
type balancer struct{ p *proxy }

func (b balancer) RoundTrip(req *http.Request) (*http.Response, error) {
	info, ok := req.Context().Value(ctxKey{}).(reqInfo)
	if !ok {
		return nil, errors.New("router: request without a route")
	}
	t := &tries{p: b.p, req: req, info: info, tried: map[*endpoint]bool{}}
	if req.Body != nil && req.Body != http.NoBody {
		t.guard = &bodyGuard{rc: req.Body}
	}
	if resp, more, err := t.run(info.pool); !more {
		return resp, err
	}
	// The table the router set while the request was tried, if it did, and
	// then the one newerTable waits for.
	if pl := b.p.poolOf(info); pl != nil && pl != info.pool {
		if resp, more, err := t.run(pl); !more {
			return resp, err
		}
	}
	if req.Context().Err() == nil && b.p.newerTable(req.Context()) {
		if pl := b.p.poolOf(info); pl != nil {
			if resp, more, err := t.run(pl); !more {
				return resp, err
			}
		}
	}
	if t.lastErr == nil {
		t.lastErr = errors.New("router: no endpoints")
	}
	return nil, t.lastErr
}

// tries is one request's tries of its route's endpoints.
type tries struct {
	p     *proxy
	req   *http.Request
	info  reqInfo
	guard *bodyGuard // the request's body, if it has one
	tried map[*endpoint]bool
	gone  []string // the addresses whose connection timed out
	fresh bool     // a connection failed: the next tries dial anew
	// lastErr is the last try's error.
	lastErr error
}

// run tries the endpoints of pl that the request has not tried, in the
// pool's order, until one takes it. It returns the reply, or the error that
// ends the request; or, with more set, nothing, when every one of them
// failed before it took the request and the request may still go to
// another.
func (t *tries) run(pl *pool) (*http.Response, bool, error) {
	req, info := t.req, t.info
	for _, e := range pl.order(t.p.now()) {
		host := addrHost(e.addr)
		if t.tried[e] || slices.Contains(t.gone, host) {
			continue
		}
		t.tried[e] = true
		out := req.Clone(t.p.tracked(req.Context()))
		out.URL.Host = e.addr
		if t.guard != nil {
			out.Body = t.guard
		}
		tr := e.tr
		if t.fresh {
			tr = e.freshTransport()
		}
		resp, err := tr.RoundTrip(out)
		if err == nil {
			e.ok()
			return resp, false, nil
		}
		t.lastErr = err
		if timedOut(err) {
			t.gone = append(t.gone, host)
			n := t.p.machineDown(host, t.p.now())
			t.p.metrics.upstreamError(info.host, "connection")
			if !retryable(req, err) || t.guard != nil {
				return nil, false, err
			}
			t.fresh = true
			t.p.log.Debug("replica's connection timed out before it replied; its address is marked down; trying another address",
				"host", info.host, "endpoint", e.addr, "endpoints_down", n, "err", err)
			continue
		}
		if !isDialError(err) {
			// A connection that failed before any reply is tried on the
			// next replica too, for a request that can be sent twice: a
			// method that is safe to repeat, and no body. This is nginx's
			// proxy_next_upstream default (error, for idempotent methods).
			// Other failures are a 502.
			if !retryable(req, err) || t.guard != nil {
				return nil, false, err
			}
			t.fresh = true
			t.p.metrics.upstreamError(info.host, "connection")
			e.failed(t.p.now())
			t.p.log.Debug("replica's connection failed before it replied; trying the next", "host", info.host, "endpoint", e.addr, "err", err)
			continue
		}
		skip := e.failed(t.p.now())
		t.p.metrics.upstreamError(info.host, "dial")
		t.p.log.Debug("replica unreachable; trying the next", "host", info.host, "endpoint", e.addr, "skipped_for", skip, "err", err)
		if t.guard != nil && t.guard.read.Load() {
			return nil, false, err
		}
	}
	return nil, true, nil
}

// poolOf returns the endpoints of the request's route in the table now, or
// nil when the route is not in it with the request's protocol. Each table
// has its own pools, so the pool is the request's own while the table is.
func (p *proxy) poolOf(info reqInfo) *pool {
	r := p.lookup(info.host).find(info.path)
	if r == nil || r.h2c != info.h2c {
		return nil
	}
	return r.pool
}

// newerTable waits for a newer table, for a request that no endpoint of its
// route took, and reports whether one may be set: the request then tries
// its route's endpoints there. On the server's router at its start, before
// its table has the server's own replicas, it waits for that table
// (awaitWhole). Otherwise it reads the table again if the table is at least
// staleAfter old, under refresh's limit of one read a second, which every
// request that comes meanwhile waits for too; a younger table is taken as
// current, and nothing is waited for.
func (p *proxy) newerTable(ctx context.Context) bool {
	if p.awaitWhole != nil && p.awaitWhole(ctx) {
		return ctx.Err() == nil
	}
	if p.refresh == nil || p.now().Sub(time.Unix(0, p.tableAt.Load())) < p.staleAfter {
		return false
	}
	p.refresh(ctx)
	return ctx.Err() == nil
}

// A request's connection to a machine that lost power fails at
// TCP_USER_TIMEOUT, and http.Transport, for a GET on a connection it had
// pooled, then sends it again on a new connection to the same replica, whose
// dial takes the whole dial timeout to fail: in the second scale run, 7.5 s
// for a request that met one dead replica. So each try carries an attempt in
// its context, and the endpoint's dialer refuses at once, with
// errMachineTimedOut, the dial of a try that wrote its request on a pooled
// connection at least half the user timeout ago: that connection timed out.
// A pooled connection that the replica had closed fails as soon as the
// request is written, and its new dial goes ahead.

// errMachineTimedOut is the dial error of a transport's second go at a
// request whose pooled connection timed out.
var errMachineTimedOut = fmt.Errorf("the connection to the replica's machine timed out: %w", syscall.ETIMEDOUT)

type attemptKey struct{}

// attempt is one try of a request on one endpoint.
type attempt struct {
	reused atomic.Bool
	wrote  atomic.Int64 // unix nanoseconds: the request was written on a pooled connection
}

// tracked returns ctx with a new attempt, which a trace keeps up to date.
func (p *proxy) tracked(ctx context.Context) context.Context {
	a := &attempt{}
	ctx = context.WithValue(ctx, attemptKey{}, a)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { a.reused.Store(i.Reused) },
		WroteRequest: func(httptrace.WroteRequestInfo) {
			if a.reused.Load() {
				a.wrote.CompareAndSwap(0, p.now().UnixNano())
			}
		},
	})
}

// dialFor wraps an endpoint's dial: see errMachineTimedOut.
func (p *proxy) dialFor(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if a, ok := ctx.Value(attemptKey{}).(*attempt); ok {
			if w := a.wrote.Load(); w != 0 && p.now().Sub(time.Unix(0, w)) >= p.userTimeout/2 {
				return nil, &net.OpError{Op: "dial", Net: network, Err: errMachineTimedOut}
			}
		}
		return dial(ctx, network, addr)
	}
}

// timedOut reports whether err is a connection to a replica that timed out,
// after it was made, or before the transport's second go at the request.
func timedOut(err error) bool {
	return errors.Is(err, errMachineTimedOut) || (!isDialError(err) && errors.Is(err, syscall.ETIMEDOUT))
}

// machineDown marks every endpoint at host down at now, and closes its idle
// connections, which go to the same machine: one connection to it timed
// out. It returns how many it marked.
func (p *proxy) machineDown(host string, now time.Time) int {
	p.mu.RLock()
	var on []*endpoint
	for k, e := range p.endpoints {
		if addrHost(k.addr) == host {
			on = append(on, e)
		}
	}
	p.mu.RUnlock()
	for _, e := range on {
		e.failed(now)
		e.tr.CloseIdleConnections()
	}
	return len(on)
}

// addrHost is the host of an address:port.
func addrHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
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
