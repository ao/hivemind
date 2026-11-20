package ha

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/ao/hivemind/internal/pki"
)

// The server port (docs/ha.md#the-transport). One TLS 1.3 listener, with a
// client certificate required, of the server role, from a server the Raft
// configuration names. ALPN tells Raft's traffic from HTTP that a follower
// passes to the leader.
const (
	// ProtoRaft is Raft's own traffic.
	ProtoRaft = "hivemind-raft/1"
	// ProtoForward is HTTP from one server to another.
	ProtoForward = "hivemind-forward/1"
)

// Handshake and dial limits on the server port.
const (
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 3 * time.Second
)

// peerCheck decides whether a server port peer is taken: its certificate
// has the server role, a node ID, and the node is allowed now.
type peerCheck func(id string) error

// tlsMaterial is what the server port presents and checks.
type tlsMaterial struct {
	cert  *pki.CertHolder
	trust *pki.TrustHolder
	allow peerCheck
}

// verifyServerPeer is the VerifyPeerCertificate of both sides: the chain is
// verified already; the leaf must carry the server role and a node ID that
// allow takes.
func (m *tlsMaterial) verifyServerPeer(_ [][]byte, chains [][]*x509.Certificate) error {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return pki.ErrNoPeerCert
	}
	leaf := chains[0][0]
	id, err := pki.NodeIDFromCert(leaf)
	if err != nil {
		return err
	}
	if !pki.HasServerRole(leaf) {
		return fmt.Errorf("%w: node %s", pki.ErrNotServer, id)
	}
	return m.allow(id)
}

func (m *tlsMaterial) serverConfig() *tls.Config {
	conf := func(pool *x509.CertPool) *tls.Config {
		return &tls.Config{
			GetCertificate:        m.cert.GetCertificate,
			ClientAuth:            tls.RequireAndVerifyClientCert,
			ClientCAs:             pool,
			MinVersion:            tls.VersionTLS13,
			NextProtos:            []string{ProtoRaft, ProtoForward},
			VerifyPeerCertificate: m.verifyServerPeer,
		}
	}
	base := conf(m.trust.Load().Pool())
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return conf(m.trust.Load().Pool()), nil
	}
	return base
}

// clientConfig is the config for dialing another server at addr for proto.
// The peer's certificate must name addr's host, as a worker checks the
// server it dials.
func (m *tlsMaterial) clientConfig(addr, proto string) (*tls.Config, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		GetClientCertificate:  m.cert.GetClientCertificate,
		RootCAs:               m.trust.Load().Pool(),
		ServerName:            host,
		MinVersion:            tls.VersionTLS13,
		NextProtos:            []string{proto},
		VerifyPeerCertificate: m.verifyServerPeer,
	}, nil
}

// dial opens a connection to the server at addr for proto, and checks that
// the peer agreed to it.
func (m *tlsMaterial) dial(ctx context.Context, addr, proto string) (*tls.Conn, error) {
	cfg, err := m.clientConfig(addr, proto)
	if err != nil {
		return nil, err
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: dialTimeout, KeepAlive: 15 * time.Second, Control: setUserTimeout}, Config: cfg}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := c.(*tls.Conn)
	if got := tc.ConnectionState().NegotiatedProtocol; got != proto {
		tc.Close()
		return nil, fmt.Errorf("the server at %s does not speak %s (it chose %q): is it a hivemind server of 2.3.0 or later?", addr, proto, got)
	}
	return tc, nil
}

// mux is the server port's listener. It completes each TLS handshake and
// hands the connection to Raft or to the forward server by its protocol.
type mux struct {
	ln       net.Listener
	tls      *tls.Config
	log      *slog.Logger
	raft     *connQueue
	forward  *connQueue
	closeErr sync.Once
	done     chan struct{}
}

func newMux(ln net.Listener, m *tlsMaterial, log *slog.Logger, advertise net.Addr) *mux {
	return &mux{ln: ln, tls: m.serverConfig(), log: log, done: make(chan struct{}),
		raft: newConnQueue(advertise), forward: newConnQueue(ln.Addr())}
}

func (x *mux) serve() {
	for {
		c, err := x.ln.Accept()
		if err != nil {
			select {
			case <-x.done:
			default:
				x.log.Error("the server port stopped accepting", "err", err)
			}
			x.raft.close()
			x.forward.close()
			return
		}
		go x.handshake(c)
	}
}

func (x *mux) handshake(c net.Conn) {
	tc := tls.Server(c, x.tls)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		x.log.Debug("server port handshake refused", "remote", c.RemoteAddr().String(), "err", err)
		c.Close()
		return
	}
	switch tc.ConnectionState().NegotiatedProtocol {
	case ProtoRaft:
		x.raft.put(tc)
	case ProtoForward:
		// Not as a *tls.Conn: http.Server closes one whose protocol it has no
		// handler for. The peer it was checked as goes with it (peerState).
		x.forward.put(&stateConn{Conn: tc, state: tc.ConnectionState()})
	default:
		tc.Close()
	}
}

func (x *mux) close() error {
	var err error
	x.closeErr.Do(func() {
		close(x.done)
		err = x.ln.Close()
	})
	return err
}

// connQueue is a net.Listener fed by the mux.
type connQueue struct {
	addr   net.Addr
	ch     chan net.Conn
	once   sync.Once
	closed chan struct{}
}

func newConnQueue(addr net.Addr) *connQueue {
	return &connQueue{addr: addr, ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (q *connQueue) put(c net.Conn) {
	select {
	case q.ch <- c:
	case <-q.closed:
		c.Close()
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.closed:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) close() { q.once.Do(func() { close(q.closed) }) }

func (q *connQueue) Close() error { q.close(); return nil }

func (q *connQueue) Addr() net.Addr { return q.addr }

// streamLayer is Raft's network transport's stream layer over the mux.
type streamLayer struct {
	*connQueue
	m *tlsMaterial
}

func (s *streamLayer) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.m.dial(ctx, string(addr), ProtoRaft)
}

// serverAddr is a server's address on the server port, as the Raft
// configuration names it: its advertise address and the port.
type serverAddr string

func (a serverAddr) Network() string { return "tcp" }
func (a serverAddr) String() string  { return string(a) }

// RaftAddr is the server port address of a server that advertises host.
func RaftAddr(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// listenServerPort binds the server port on ip, which must not be a
// wildcard, as the other cluster listeners refuse one.
func listenServerPort(ip string, port int) (net.Listener, error) {
	if a := net.ParseIP(ip); a == nil || a.IsUnspecified() {
		return nil, fmt.Errorf("the server port needs one IP address of this machine, not %q", ip)
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || tcp.IP.IsUnspecified() {
		ln.Close()
		return nil, fmt.Errorf("the server port bound %s, a wildcard address", ln.Addr())
	}
	return ln, nil
}

// observedTransport is raft's network transport, noting when each peer
// last answered the leader's AppendEntries, heartbeats included: the
// "last contact" hivemind servers shows. An answer also ends the limit on
// the peer's failure lines (peerlog.go), if any.
type observedTransport struct {
	*raft.NetworkTransport
	mu    sync.Mutex
	seen  map[raft.ServerID]time.Time
	fails *peerFailures
}

func (t *observedTransport) AppendEntries(id raft.ServerID, target raft.ServerAddress, args *raft.AppendEntriesRequest,
	resp *raft.AppendEntriesResponse) error {
	err := t.NetworkTransport.AppendEntries(id, target, args, resp)
	if err == nil {
		t.mu.Lock()
		t.seen[id] = time.Now()
		t.mu.Unlock()
		if t.fails != nil {
			t.fails.answered(string(target))
		}
	}
	return err
}

func (t *observedTransport) lastSeen(id raft.ServerID) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[id]
}

// stateConn is a server port connection of the forward protocol, with the
// TLS state of its handshake.
type stateConn struct {
	net.Conn
	state tls.ConnectionState
}

type peerStateKey struct{}

// connContext puts a forward connection's TLS state in the context of its
// requests (http.Server.ConnContext).
func connContext(ctx context.Context, c net.Conn) context.Context {
	if sc, ok := c.(*stateConn); ok {
		st := sc.state
		return context.WithValue(ctx, peerStateKey{}, &st)
	}
	return ctx
}

// peerState is the TLS state of the forward connection ctx belongs to.
func peerState(ctx context.Context) *tls.ConnectionState {
	st, _ := ctx.Value(peerStateKey{}).(*tls.ConnectionState)
	return st
}
