package router

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// Connection limits on the router's listeners. See Config for what each one
// does.
const (
	// DefaultMaxConnsPerAddr is the default cap of one client address's
	// connections, over both listeners. A browser opens a handful per
	// host; a few hundred leaves room for clients behind one NAT.
	DefaultMaxConnsPerAddr = 256
	// DefaultWriteTimeout is how long a write to an https client may make
	// no progress before the connection ends.
	DefaultWriteTimeout = 60 * time.Second
	// DefaultUpgradeIdleTimeout is how long an upgraded connection, such as
	// a WebSocket, or a request on an h2c route, may carry no data either
	// way before it is closed.
	DefaultUpgradeIdleTimeout = 10 * time.Minute

	// fdReserve is the file descriptors DefaultMaxConns leaves to the rest
	// of the process: on the server, the store, the API, the node and join
	// listeners and the Docker socket.
	fdReserve = 1024
	// fallbackMaxConns is DefaultMaxConns where the open-file limit is not
	// known, as on Windows.
	fallbackMaxConns = 16384
)

// DefaultMaxConns is the default total cap of the router's connections,
// from the process's open-file limit (RLIMIT_NOFILE): half of what is left
// after fdReserve, as a proxied connection holds two descriptors, the
// client's and, while a request runs, the replica's. With a limit of 2048
// or less it is a quarter of the limit.
func DefaultMaxConns() int {
	n := openFileLimit()
	switch {
	case n <= 0:
		return fallbackMaxConns
	case n <= 2*fdReserve:
		return max(n/4, 1)
	}
	return (n - fdReserve) / 2
}

// Reasons a connection is refused, the reason label of
// hivemind_router_connections_refused_total.
const (
	refusedAddr  = "address"
	refusedTotal = "total"
)

// connLimits counts the router's open connections, in total and by client
// address, across both listeners. An IPv4 client is its address; an IPv6
// client is its /64, since one host commonly has a whole /64 to pick
// addresses from.
type connLimits struct {
	total, perAddr int // a cap below 1 is no cap
	log            *slog.Logger
	metrics        *Metrics

	mu     sync.Mutex
	n      int
	byAddr map[netip.Prefix]int
	// conns holds every connection accepted and not closed yet, hijacked
	// ones included, which the router's Shutdown closes at the end of its
	// wait (closeAll).
	conns map[*limitConn]struct{}
}

func newConnLimits(total, perAddr int, log *slog.Logger, m *Metrics) *connLimits {
	return &connLimits{total: total, perAddr: perAddr, log: log, metrics: m, byAddr: map[netip.Prefix]int{},
		conns: map[*limitConn]struct{}{}}
}

// clientKey is what the per-address cap counts a connection from addr by.
func clientKey(addr net.Addr) netip.Prefix {
	var ip netip.Addr
	if ta, ok := addr.(*net.TCPAddr); ok {
		ip = ta.AddrPort().Addr()
	} else if ap, err := netip.ParseAddrPort(addr.String()); err == nil {
		ip = ap.Addr()
	}
	ip = ip.Unmap().WithZone("")
	if !ip.Is6() {
		return netip.PrefixFrom(ip, ip.BitLen())
	}
	p, _ := ip.Prefix(64)
	return p
}

// admit counts a new connection from addr, or returns the reason it is
// over a cap.
func (l *connLimits) admit(addr net.Addr) (netip.Prefix, string) {
	key := clientKey(addr)
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.total > 0 && l.n >= l.total:
		return key, refusedTotal
	case l.perAddr > 0 && l.byAddr[key] >= l.perAddr:
		return key, refusedAddr
	}
	l.n++
	l.byAddr[key]++
	return key, ""
}

// track records c, an admitted connection, as open.
func (l *connLimits) track(c *limitConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conns[c] = struct{}{}
}

// release gives back the count of c, which is closed.
func (l *connLimits) release(c *limitConn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.byAddr[c.key]--; l.byAddr[c.key] <= 0 {
		delete(l.byAddr, c.key)
	}
	delete(l.conns, c)
}

// closeAll closes every connection still open, and returns how many it
// closed.
func (l *connLimits) closeAll() int {
	l.mu.Lock()
	open := make([]*limitConn, 0, len(l.conns))
	for c := range l.conns {
		open = append(open, c)
	}
	l.mu.Unlock()
	for _, c := range open {
		_ = c.Close()
	}
	return len(open)
}

// open returns the number of connections open now.
func (l *connLimits) open() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// limitListener applies limits to the connections it accepts. One over a
// cap is closed at once, logged at debug level and counted; the listener
// goes on to the next one. A connection it returns gives its count back
// when it is closed, and, with writeTimeout set, ends when a write makes no
// progress for that long.
type limitListener struct {
	net.Listener
	limits       *connLimits
	writeTimeout time.Duration
}

func (ln *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := ln.Listener.Accept()
		if err != nil {
			return nil, err
		}
		key, reason := ln.limits.admit(c.RemoteAddr())
		if reason == "" {
			lc := &limitConn{Conn: c, limits: ln.limits, key: key, writeTimeout: ln.writeTimeout}
			ln.limits.track(lc)
			return lc, nil
		}
		// Count it before the close, so that a client that sees the
		// connection end also sees it counted.
		ln.limits.metrics.connRefused(reason)
		_ = c.Close()
		ln.limits.log.Debug("connection refused: over the router's limit", "remote", c.RemoteAddr().String(), "limit", reason)
	}
}

// limitConn is an accepted connection under limits.
type limitConn struct {
	net.Conn
	limits       *connLimits
	key          netip.Prefix
	writeTimeout time.Duration
	once         sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.limits.release(c) })
	return err
}

// Write sets the write deadline writeTimeout ahead before each write, and
// again whenever a write that reached its deadline had written something:
// a client that reads nothing for writeTimeout ends the connection, and one
// that reads slowly, or a stream that is idle between writes, does not.
// Nothing in the router sets its own write deadline on an https connection:
// its server has no WriteTimeout.
func (c *limitConn) Write(b []byte) (int, error) {
	if c.writeTimeout <= 0 {
		return c.Conn.Write(b)
	}
	done := 0
	for {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			return done, err
		}
		n, err := c.Conn.Write(b[done:])
		done += n
		if err == nil || n == 0 || done == len(b) || !errors.Is(err, os.ErrDeadlineExceeded) {
			return done, err
		}
	}
}

// idleBody is the replica's side of an upgraded connection, such as a
// WebSocket. Everything the client sends is written to it and everything the
// replica sends is read from it, so it sees the data both ways; when none
// passes for idle, it is closed, and ReverseProxy then closes the client's
// side too.
type idleBody struct {
	io.ReadWriteCloser
	idle time.Duration
	log  *slog.Logger
	host string

	mu     sync.Mutex
	last   time.Time
	timer  *time.Timer
	closed bool
}

func newIdleBody(rwc io.ReadWriteCloser, idle time.Duration, log *slog.Logger, host string) *idleBody {
	b := &idleBody{ReadWriteCloser: rwc, idle: idle, log: log, host: host, last: time.Now()}
	b.mu.Lock()
	b.timer = time.AfterFunc(idle, b.check)
	b.mu.Unlock()
	return b
}

func (b *idleBody) touch() {
	b.mu.Lock()
	b.last = time.Now()
	b.mu.Unlock()
}

func (b *idleBody) check() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	if since := time.Since(b.last); since < b.idle {
		b.timer.Reset(b.idle - since)
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	b.log.Debug("closing an upgraded connection that carried no data", "host", b.host, "idle", b.idle)
	_ = b.ReadWriteCloser.Close()
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Read(p)
	if n > 0 {
		b.touch()
	}
	return n, err
}

func (b *idleBody) Write(p []byte) (int, error) {
	n, err := b.ReadWriteCloser.Write(p)
	if n > 0 {
		b.touch()
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.ReadWriteCloser.Close()
}

// upgradeType is the protocol a request or response asks to switch to, as
// ReverseProxy reads it: Upgrade, when Connection names it.
func upgradeType(h http.Header) string {
	for _, v := range h.Values("Connection") {
		for tok := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return h.Get("Upgrade")
			}
		}
	}
	return ""
}

// errWrongUpgrade is a 101 for another protocol than the request named.
var errWrongUpgrade = errors.New("router: the replica switched to another protocol than the request asked for")

// modifyResponse is the proxy's ModifyResponse. A 101 for the protocol the
// request asked for gets the idle limit; one for another protocol, or for
// none, is refused, which closes the replica's connection and answers 502.
// The reply to a request on an h2c route, which HTTP/2 never makes a 101,
// counts towards that request's idle limit as it is read.
func (p *proxy) modifyResponse(res *http.Response) error {
	if info, _ := res.Request.Context().Value(ctxKey{}).(reqInfo); info.stream != nil {
		info.stream.touch()
		res.Body = &touchBody{ReadCloser: res.Body, s: info.stream}
		return nil
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		return nil
	}
	want, got := upgradeType(res.Request.Header), upgradeType(res.Header)
	if want == "" || !strings.EqualFold(want, got) {
		return errWrongUpgrade
	}
	rwc, ok := res.Body.(io.ReadWriteCloser)
	if !ok || p.upgradeIdle <= 0 {
		return nil
	}
	info, _ := res.Request.Context().Value(ctxKey{}).(reqInfo)
	res.Body = newIdleBody(rwc, p.upgradeIdle, p.log, info.host)
	return nil
}
