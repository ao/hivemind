package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// rotFixture is a server whose CAs a carotate.Manager holds, with both
// listeners on loopback.
type rotFixture struct {
	t        *testing.T
	dir      string
	st       *store.Store
	m        *carotate.Manager
	tokens   *pki.Tokens
	nodeAddr string
	joinAddr string
}

func newRotFixture(t *testing.T) *rotFixture {
	t.Helper()
	dir := t.TempDir()
	if _, err := pki.CreateCA(dir); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := carotate.Open(context.Background(), carotate.Options{Dir: dir, Store: st, LocalNode: "node-server", Logger: log})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	tokens := pki.NewTokens(st.JoinTokens())
	c, err := Listen(Config{Store: st, Rotation: m, Tokens: tokens, Advertise: "127.0.0.1", Port: 0,
		LocalNodeID: "node-server", Logger: log})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = c.Serve() }()
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = c.Shutdown()
		st.Close()
	})
	return &rotFixture{t: t, dir: dir, st: st, m: m, tokens: tokens, nodeAddr: c.NodeAddr(), joinAddr: c.JoinAddr()}
}

// join joins with a token for the CA with fingerprint fp, pinned to it. It
// returns the node ID, its certificate and the CA of the reply.
func (fx *rotFixture) join(fp [32]byte) (string, tls.Certificate, *x509.Certificate, error) {
	fx.t.Helper()
	tok, _, err := fx.tokens.Issue(fp, time.Hour)
	if err != nil {
		fx.t.Fatal(err)
	}
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	reply, err := nodeapi.Join(context.Background(), fx.joinAddr, tok, csr)
	if err != nil {
		return "", tls.Certificate{}, nil, err
	}
	cb, _ := pem.Decode([]byte(reply.CAPEM))
	ca, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		fx.t.Fatal(err)
	}
	nb, _ := pem.Decode([]byte(reply.CertPEM))
	cert, err := pki.NewTLSCertificate(nb.Bytes, key, &pki.CA{Cert: ca})
	if err != nil {
		fx.t.Fatal(err)
	}
	return reply.NodeID, cert, ca, nil
}

// worker is a node API client that trusts trust, presents cert, says it
// trusts trust's CAs, and records the last CA digest it saw.
type worker struct {
	c      *nodeapi.Client
	id     string
	digest atomic.Value
}

func (fx *rotFixture) worker(id string, trust *pki.Trust, cert tls.Certificate) *worker {
	fx.t.Helper()
	cfg, err := pki.ClientTLSConfigTrust(trust, pki.NewCertHolder(cert), "127.0.0.1")
	if err != nil {
		fx.t.Fatal(err)
	}
	c, err := nodeapi.NewClient("https://"+fx.nodeAddr, cfg,
		nodeapi.WithCATrust(func() string { return nodeapi.FormatCATrust(trust.Fingerprints()) }))
	if err != nil {
		fx.t.Fatal(err)
	}
	w := &worker{c: c, id: id}
	c.OnCA(func(d string) { w.digest.Store(d) })
	return w
}

func (w *worker) register() error {
	return w.c.Register(context.Background(), w.id, nodeapi.Register{Name: w.id, CPU: 1, Memory: 1 << 30})
}

func (w *worker) lastDigest() string { s, _ := w.digest.Load().(string); return s }

// renew renews w's certificate and returns the new one.
func (w *worker) renew(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	certPEM, err := w.c.Renew(context.Background(), w.id, csr)
	if err != nil {
		t.Fatalf("renew %s: %v", w.id, err)
	}
	b, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: key, Leaf: leaf}
}

func issuedBy(cert tls.Certificate, ca *pki.CA) bool {
	return cert.Leaf.CheckSignatureFrom(ca.Cert) == nil
}

// serverCertFrom returns which of old and next the join listener's
// certificate, the server's own, verifies against for 127.0.0.1, or nil.
func (fx *rotFixture) serverCertFrom(old, next *pki.CA) *pki.CA {
	fx.t.Helper()
	for _, ca := range []*pki.CA{old, next} {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", fx.joinAddr,
			&tls.Config{RootCAs: pki.MustTrust(ca.Cert).Pool(), ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13})
		if err == nil {
			conn.Close()
			return ca
		}
	}
	return nil
}

// A rotation over the node listener, with real TLS: what the server
// accepts, issues and presents in each phase, and what a worker is told.
func TestRotationOverTheNodeListener(t *testing.T) {
	fx := newRotFixture(t)
	ctx := context.Background()
	old := fx.m.Current()
	oldOnly := pki.MustTrust(old.Cert)

	idA, certA, caA, err := fx.join(old.Fingerprint())
	if err != nil || pki.Fingerprint(caA) != old.Fingerprint() {
		t.Fatalf("join before the rotation: %v", err)
	}
	a := fx.worker(idA, oldOnly, certA)
	if err := a.register(); err != nil {
		t.Fatal(err)
	}
	d0 := a.lastDigest()
	if !nodeapi.ValidCADigest(d0) {
		t.Fatalf("no CA digest on a reply: %q", d0)
	}

	// start: the digest changes, and the set has both CAs.
	if _, err := fx.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := fx.m.Status(ctx)
	next, err := pki.LoadNextCA(fx.dir)
	if err != nil || pki.FingerprintHex(next.Fingerprint()) != st.Next.Fingerprint {
		t.Fatalf("the next CA: %v", err)
	}
	both := pki.MustTrust(old.Cert, next.Cert)
	if err := a.register(); err != nil {
		t.Fatal(err)
	}
	if a.lastDigest() == d0 {
		t.Fatal("the CA digest did not change at start")
	}
	b, root, err := a.c.CABundle(ctx, idA)
	if err != nil || pki.Fingerprint(root) != old.Fingerprint() || len(b.CAs) != 2 || b.Phase != nodeapi.CAPhaseTrust {
		t.Fatalf("the CA set in phase trust: %+v %v", b, err)
	}
	// A join token for the new CA fails at the pin: the server presents the old.
	if _, _, _, err := fx.join(next.Fingerprint()); !errors.Is(err, pki.ErrFingerprintMismatch) {
		t.Fatalf("a token for the new CA in phase trust: %v", err)
	}
	// node-a says it trusts both; in phase trust its renewal is from the old CA.
	a2 := fx.worker(idA, both, certA)
	if err := a2.register(); err != nil {
		t.Fatal(err)
	}
	if c := a2.renew(t); !issuedBy(c, old) {
		t.Fatal("phase trust: a renewal from the new CA")
	}
	// A node joined now, which says nothing of what it trusts: a build
	// without rotation.
	idC, certC, _, err := fx.join(old.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	cfgC, _ := pki.ClientTLSConfigTrust(both, pki.NewCertHolder(certC), "127.0.0.1")
	oldBuild, _ := nodeapi.NewClient("https://"+fx.nodeAddr, cfgC)
	if err := oldBuild.Register(ctx, idC, nodeapi.Register{Name: idC, CPU: 1, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.m.Renew(ctx, false); err == nil {
		t.Fatal("renew with a live build without rotation")
	}
	if _, err := fx.m.Renew(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Phase renew: a node that trusts the new CA renews under it; one that
	// says nothing renews under the old.
	newA := a2.renew(t)
	if !issuedBy(newA, next) {
		t.Fatal("phase renew: a node that trusts the new CA renewed under the old")
	}
	if c, err := renewWith(oldBuild, idC); err != nil || !issuedBy(c, old) {
		t.Fatalf("phase renew: the old build's renewal: %v", err)
	}
	// The server's certificate stays while the old build does not trust
	// the new CA.
	time.Sleep(200 * time.Millisecond)
	if fx.serverCertFrom(old, next) != old {
		t.Fatal("the server's certificate moved while a node does not trust the new CA")
	}

	// node-a presents its new certificate; its old one is refused after.
	a3 := fx.worker(idA, both, newA)
	if err := a3.register(); err != nil {
		t.Fatal(err)
	}
	if err := a2.register(); !errors.Is(err, nodeapi.ErrCASuperseded) {
		t.Fatalf("node-a's old certificate after it renewed: %v, want ErrCASuperseded", err)
	}

	// The old build is removed: the server's certificate moves.
	if _, err := fx.st.RemoveNode(ctx, idC, true); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for fx.serverCertFrom(old, next) != next {
		if time.Now().After(deadline) {
			t.Fatal("the server's certificate did not move to the new CA")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// A worker that trusts only the old CA cannot reach it now; one that
	// trusts only the new can.
	if err := fx.worker(idA, oldOnly, newA).register(); err == nil {
		t.Fatal("a worker trusting only the old CA reached a server with a certificate from the new")
	}
	if err := fx.worker(idA, pki.MustTrust(next.Cert), newA).register(); err != nil {
		t.Fatalf("a worker trusting only the new CA: %v", err)
	}
	// Join tokens: one for the old CA fails at the pin, one for the new
	// joins, with a certificate and a CA from the new CA.
	if _, _, _, err := fx.join(old.Fingerprint()); !errors.Is(err, pki.ErrFingerprintMismatch) {
		t.Fatalf("a token for the old CA after the move: %v", err)
	}
	idB, certB, caB, err := fx.join(next.Fingerprint())
	if err != nil || pki.Fingerprint(caB) != next.Fingerprint() || !issuedBy(certB, next) {
		t.Fatalf("a token for the new CA after the move: %v", err)
	}
	wb := fx.worker(idB, pki.MustTrust(next.Cert), certB)
	if err := wb.register(); err != nil {
		t.Fatal(err)
	}

	// A node with a certificate from the old CA that keeps a connection
	// open across finish is refused at its next request on it.
	// (node-stale stands for one that joined before the move.)
	idE, certE := joinOld(t, fx, old)
	we := fx.worker(idE, both, certE)
	if err := we.register(); err != nil {
		t.Fatalf("a node with an old-CA certificate before finish: %v", err)
	}
	if _, err := fx.m.Finish(ctx, false); err == nil {
		t.Fatal("finish with a node that has not renewed")
	}
	if _, err := fx.m.Finish(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := we.register(); !errors.Is(err, nodeapi.ErrCAUntrusted) {
		t.Fatalf("an old-CA certificate on a connection from before finish: %v, want ErrCAUntrusted", err)
	}
	if err := fx.worker(idE, both, certE).register(); err == nil {
		t.Fatal("a new connection with an old-CA certificate after finish")
	}
	if err := a3.register(); err != nil {
		t.Fatalf("a renewed node after finish: %v", err)
	}
	b, _, err = a3.c.CABundle(ctx, idA)
	if err != nil || len(b.CAs) != 1 || b.Phase != "" {
		t.Fatalf("the CA set after finish: %+v %v", b, err)
	}
}

// joinOld enrolls a node with a certificate from the old CA, as one that
// joined before the server's certificate moved: the store issues it
// directly, since the join listener presents the new CA by now.
func joinOld(t *testing.T, fx *rotFixture, old *pki.CA) (string, tls.Certificate) {
	t.Helper()
	id := "node-stale"
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	der, err := old.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, old)
	if err != nil {
		t.Fatal(err)
	}
	return id, cert
}

func renewWith(c *nodeapi.Client, id string) (tls.Certificate, error) {
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	certPEM, err := c.Renew(context.Background(), id, csr)
	if err != nil {
		return tls.Certificate{}, err
	}
	b, _ := pem.Decode(certPEM)
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{b.Bytes}, PrivateKey: key, Leaf: leaf}, nil
}

// A listener given a CA alone still sends the digest of its one-CA set.
func TestFixedCASendsItsDigest(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	c := fx.nodeClient(cert)
	req, _ := http.NewRequest(http.MethodGet, fx.nodeURL(nodeapi.CAPath(id)), nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b nodeapi.CABundle
	if err := nodeapi.Decode(resp.Body, nodeapi.MaxCABytes, &b); err != nil || len(b.CAs) != 1 {
		t.Fatalf("the CA set: %+v %v", b, err)
	}
	if got := resp.Header.Get(nodeapi.CAHeader); got != b.Digest() {
		t.Fatalf("digest %q, want %q", got, b.Digest())
	}
}
