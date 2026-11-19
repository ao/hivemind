package cluster

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// The node listener does each connection's TLS handshake itself, before
// http.Server gets the connection, so that a failed handshake can be told
// apart by its error. http.Server's own handshake logs every failure as
// "http: TLS handshake error from <address>: ...", at info, through the
// standard log. In the fifth soak a worker cut off by finish --force showed
// on the server as that line alone, unnamed, every 5 s, about 17000 lines a
// day. The handshake is the same one, with the same config: a certificate
// the listener does not accept is still refused.

// handshakeListener accepts connections from ln, does their TLS handshake
// with cfg in a goroutine each, within timeout, and hands the connections
// whose handshake passed to Accept. A failed handshake goes to failed, and
// its connection is closed.
type handshakeListener struct {
	net.Listener
	cfg     *tls.Config
	timeout time.Duration
	failed  func(remote net.Addr, err error)

	ready chan net.Conn
	errs  chan error
	done  chan struct{}
	once  sync.Once
}

func newHandshakeListener(ln net.Listener, cfg *tls.Config, timeout time.Duration, failed func(net.Addr, error)) *handshakeListener {
	l := &handshakeListener{Listener: ln, cfg: cfg, timeout: timeout, failed: failed,
		ready: make(chan net.Conn), errs: make(chan error), done: make(chan struct{})}
	go l.run()
	return l
}

// run accepts until the listener fails. An error goes to Accept; after one
// that is not temporary, as when the listener is closed, it stops.
func (l *handshakeListener) run() {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.errs <- err:
			case <-l.done:
				return
			}
			if !isTemporary(err) {
				return
			}
			continue
		}
		go l.handshake(c)
	}
}

// isTemporary is http.Server's test for an Accept error to retry.
func isTemporary(err error) bool {
	t, ok := err.(interface{ Temporary() bool })
	return ok && t.Temporary()
}

func (l *handshakeListener) handshake(c net.Conn) {
	tc := tls.Server(c, l.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), l.timeout)
	defer cancel()
	_ = tc.SetDeadline(time.Now().Add(l.timeout))
	if err := tc.HandshakeContext(ctx); err != nil {
		l.failed(c.RemoteAddr(), err)
		_ = tc.Close()
		return
	}
	_ = tc.SetDeadline(time.Time{})
	select {
	case l.ready <- tc:
	case <-l.done:
		_ = tc.Close()
	}
}

// Accept returns the next connection whose handshake passed. http.Server
// finds it a *tls.Conn with its handshake done, and takes it as its own.
func (l *handshakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case err := <-l.errs:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *handshakeListener) Close() error {
	err := net.ErrClosed
	l.once.Do(func() {
		close(l.done)
		err = l.Listener.Close()
	})
	return err
}

// refusedLog logs the node listener's failed handshakes. Two kinds name a
// node, at warn, at most once a minute for each, as a node cut off by a
// rotation of the cluster CA makes them every few seconds:
//
//   - the client presented a certificate from a CA the cluster does not
//     trust. The node ID is read from that certificate, which is not
//     verified, and the line says so; the CA's fingerprint is that of the CA
//     certificate the client sent with it, when that one signed it.
//   - the client refused this server's certificate. A client sends its own
//     certificate only once it accepts the server's, so the node is known by
//     its address alone, the one it registered.
//
// Every other failure is logged at info, as http.Server logged it. So are
// the two kinds past refusedBurst lines a minute, so that a client that
// sends certificates with made-up node IDs cannot fill the log faster than
// before.
type refusedLog struct {
	log *slog.Logger
	now func() time.Time
	// trusted is the fingerprints of the CAs the listener accepts now.
	trusted func() [][32]byte
	// nodeAt returns the node that registered ip as its address.
	nodeAt func(ctx context.Context, ip netip.Addr) (id, name string, ok bool)

	mu     sync.Mutex
	last   map[string]time.Time // when each key was last logged at warn
	minute time.Time            // the start of the minute counted in lines
	lines  int
}

const (
	// refusedEvery is how often one node's refused handshakes are logged.
	refusedEvery = time.Minute
	// refusedKeys bounds the keys refusedLog remembers.
	refusedKeys = 1024
	// refusedBurst bounds the warn lines in a minute, over every key.
	refusedBurst = 30
)

// cutOffFix is what the warn lines say to do.
const cutOffFix = "if it is a node of this cluster, cut off by hivemind ca rotate finish --force, run hivemind node remove <id> on the server, " +
	"then join the machine again with an empty data directory and a new join token (docs/ca-rotation.md#nodes-that-are-offline)"

func (r *refusedLog) note(remote net.Addr, err error) {
	var cve *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	if errors.As(err, &cve) && errors.As(cve.Err, &ua) && len(cve.UnverifiedCertificates) > 0 {
		r.untrustedCert(remote, err, cve.UnverifiedCertificates)
		return
	}
	if refusedServerCert(err) {
		r.refusedServer(remote, err)
		return
	}
	r.log.Info("TLS handshake error on the node listener", "remote", remote.String(), "err", err)
}

// refusedServerCert reports whether the client ended the handshake because
// it did not accept the server's certificate: a Go client sends
// bad_certificate, others unknown_ca.
func refusedServerCert(err error) bool {
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "remote error" || oe.Err == nil {
		return false
	}
	msg := oe.Err.Error()
	return strings.HasSuffix(msg, "bad certificate") || strings.HasSuffix(msg, "unknown certificate authority")
}

// allow reports whether key may be logged at warn now, and records it.
func (r *refusedLog) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if last, ok := r.last[key]; ok && now.Sub(last) < refusedEvery {
		return false
	}
	if now.Sub(r.minute) >= time.Minute {
		r.minute, r.lines = now, 0
	}
	if r.lines >= refusedBurst {
		return false
	}
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	if _, ok := r.last[key]; !ok && len(r.last) >= refusedKeys {
		for k, t := range r.last {
			if now.Sub(t) >= refusedEvery {
				delete(r.last, k)
			}
		}
		if len(r.last) >= refusedKeys {
			return false
		}
	}
	r.last[key] = now
	r.lines++
	return true
}

// mark records key now without a line, within refusedKeys.
func (r *refusedLog) mark(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]time.Time{}
	}
	if _, ok := r.last[key]; ok || len(r.last) < refusedKeys {
		r.last[key] = r.now()
	}
}

// seen reports whether key was logged at warn, or marked, within
// refusedEvery.
func (r *refusedLog) seen(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.last[key]
	return ok && r.now().Sub(last) < refusedEvery
}

func (r *refusedLog) untrustedCert(remote net.Addr, err error, certs []*x509.Certificate) {
	leaf := certs[0]
	id, idErr := pki.NodeIDFromCert(leaf)
	if idErr != nil {
		id = ""
	}
	key := "node:" + id
	if id == "" {
		key = "addr:" + addrIP(remote)
	}
	if !r.allow(key) {
		if !r.seen(key) {
			r.log.Info("TLS handshake error on the node listener", "remote", remote.String(), "err", err)
		}
		return
	}
	ca, caName := "-", leaf.Issuer.CommonName
	if len(certs) > 1 && leaf.CheckSignatureFrom(certs[1]) == nil {
		ca = shortFP(pki.Fingerprint(certs[1]))
		caName = certs[1].Subject.CommonName
	}
	// The name is the client's to choose; slog quotes it, and it is cut.
	if rs := []rune(caName); len(rs) > 64 {
		caName = string(rs[:64])
	}
	sum := sha256.Sum256(leaf.Raw)
	r.log.Warn("a node presented a certificate from a CA this cluster does not trust, and was refused: it was likely cut off by a rotation "+
		"of the cluster CA. Its node ID is read from the refused certificate, which is not verified; "+cutOffFix,
		"node", firstNonEmpty(id, "-"), "node_id_verified", false, "remote", remote.String(), "ca", ca, "ca_name", caName,
		"ca_key_id", hex.EncodeToString(leaf.AuthorityKeyId), "cert", hex.EncodeToString(sum[:8]), "trusted", r.trustedList())
}

func (r *refusedLog) refusedServer(remote net.Addr, err error) {
	ip := addrIP(remote)
	key, miss := "addr:"+ip, "miss:"+ip
	if r.seen(key) {
		return
	}
	// An address that is no node's is looked up again only after a
	// minute, so that handshakes from it cost no store read each.
	var id, name string
	ok := false
	if addr, perr := netip.ParseAddr(ip); perr == nil && r.nodeAt != nil && !r.seen(miss) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		id, name, ok = r.nodeAt(ctx, addr.Unmap())
		cancel()
		if !ok {
			r.mark(miss)
		}
	}
	if !ok || !r.allow(key) {
		r.log.Info("TLS handshake error on the node listener", "remote", remote.String(), "err", err)
		return
	}
	r.log.Warn("a node refused this server's certificate, so it does not trust the CA of the certificate: it was likely cut off by a "+
		"rotation of the cluster CA. A node sends its own certificate only once it accepts the server's, so this one is known by its "+
		"address alone, the one it registered; "+cutOffFix,
		"node", id, "name", name, "remote", remote.String(), "trusted", r.trustedList())
}

func (r *refusedLog) trustedList() string {
	if r.trusted == nil {
		return ""
	}
	var out []string
	for _, fp := range r.trusted() {
		out = append(out, shortFP(fp))
	}
	return strings.Join(out, ",")
}

func addrIP(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		if ip, ok := netip.AddrFromSlice(ta.IP); ok {
			return ip.Unmap().String()
		}
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

func shortFP(fp [32]byte) string { return pki.FingerprintHex(fp)[:16] }

// storeNodeAt finds, in the store, the node that registered ip as its
// address.
func storeNodeAt(st *store.Store) func(ctx context.Context, ip netip.Addr) (string, string, bool) {
	return func(ctx context.Context, ip netip.Addr) (string, string, bool) {
		nodes, err := st.ListNodes(ctx)
		if err != nil {
			return "", "", false
		}
		for _, n := range nodes {
			if a, err := netip.ParseAddr(n.Address); err == nil && a.Unmap() == ip {
				return n.ID, n.Name, true
			}
		}
		return "", "", false
	}
}
