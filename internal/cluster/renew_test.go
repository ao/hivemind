package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// fakeClock is a clock that starts at the real time and moves only when a
// test moves it.
type fakeClock struct{ off atomic.Int64 }

func (c *fakeClock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *fakeClock) advance(d time.Duration) { c.off.Add(int64(d)) }

// newRenewFixture is newFixture with a certificate lifetime and a clock.
func newRenewFixture(t *testing.T, lifetime time.Duration, now func() time.Time) (*fixture, *Cluster) {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := pki.NewTokens(st.JoinTokens())
	c, err := Listen(Config{Store: st, CA: ca, Tokens: tokens, Advertise: "127.0.0.1", Port: 0,
		LocalNodeID: "node-server", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		NodeCertLifetime: lifetime, Now: now})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	go func() { _ = c.Serve() }()
	t.Cleanup(func() {
		_ = c.Shutdown()
		st.Close()
	})
	return &fixture{t: t, st: st, ca: ca, tokens: tokens, nodeAddr: c.NodeAddr(), joinAddr: c.JoinAddr()}, c
}

// apiClient is the worker's node API client for cert.
func (fx *fixture) apiClient(cert tls.Certificate) *nodeapi.Client {
	fx.t.Helper()
	cfg, err := pki.ClientTLSConfig(fx.ca, cert, "127.0.0.1")
	if err != nil {
		fx.t.Fatal(err)
	}
	c, err := nodeapi.NewClient("https://"+fx.nodeAddr, cfg)
	if err != nil {
		fx.t.Fatal(err)
	}
	return c
}

func newCSR(t *testing.T) (*x509.CertificateRequest, []byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := x509.ParseCertificateRequest(csr)
	return parsed, csr, key
}

func parsePEMCert(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("no PEM in %q", b)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A renewal returns a certificate for the new key, for the same node ID,
// signed by the cluster CA, and the server records its expiry.
func TestRenewIssuesSameNodeIDForNewKey(t *testing.T) {
	fx, _ := newRenewFixture(t, 0, nil)
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	ctx := context.Background()
	joined, err := fx.st.NodeCertExpiry(ctx, id)
	if err != nil || !joined.Equal(cert.Leaf.NotAfter) {
		t.Fatalf("expiry recorded at join: %s, %v; want %s", joined, err, cert.Leaf.NotAfter)
	}

	csr, csrDER, _ := newCSR(t)
	time.Sleep(1100 * time.Millisecond) // certificate times have one-second resolution
	certPEM, err := fx.apiClient(cert).Renew(ctx, id, csrDER)
	if err != nil {
		t.Fatal(err)
	}
	leaf := parsePEMCert(t, certPEM)
	got, err := pki.NodeIDFromCert(leaf)
	if err != nil || got != id {
		t.Fatalf("renewed certificate names %q (%v), want %q", got, err, id)
	}
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(csr.PublicKey) {
		t.Fatal("renewed certificate is not for the key in the request")
	}
	pool := x509.NewCertPool()
	pool.AddCert(fx.ca.Cert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("renewed certificate does not chain to the CA: %v", err)
	}
	if leaf.SerialNumber.Cmp(cert.Leaf.SerialNumber) == 0 || !leaf.NotAfter.After(cert.Leaf.NotAfter) {
		t.Fatalf("renewed certificate is not new: serial %s, not after %s", leaf.SerialNumber, leaf.NotAfter)
	}
	rec, err := fx.st.NodeCertExpiry(ctx, id)
	if err != nil || !rec.Equal(leaf.NotAfter) {
		t.Fatalf("expiry recorded after renewal: %s, %v; want %s", rec, err, leaf.NotAfter)
	}
}

// A node cannot renew for another ID: a path that names another node is
// refused, and names in the request are ignored.
func TestRenewCannotNameAnotherNode(t *testing.T) {
	fx, _ := newRenewFixture(t, 0, nil)
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	otherID, _ := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	c := fx.apiClient(cert)
	ctx := context.Background()

	_, csr, _ := newCSR(t)
	if _, err := c.Renew(ctx, otherID, csr); err == nil {
		t.Fatal("a node renewed a certificate for another node's ID")
	}

	// A request that asks for the other node's URI and a host name gets a
	// certificate for its own ID, with no host name.
	key, _ := pki.NewNodeKey()
	other, _ := url.Parse("hivemind://node/" + otherID)
	asking, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		URIs: []*url.URL{other}, DNSNames: []string{"server.example"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := c.Renew(ctx, id, asking)
	if err != nil {
		t.Fatal(err)
	}
	leaf := parsePEMCert(t, certPEM)
	if got, _ := pki.NodeIDFromCert(leaf); got != id || len(leaf.URIs) != 1 || len(leaf.DNSNames) != 0 {
		t.Fatalf("renewed certificate names %q, URIs %v, DNS %v; want only %q", got, leaf.URIs, leaf.DNSNames, id)
	}
}

// A removed node cannot renew, and gets the removal error.
func TestRemovedNodeCannotRenew(t *testing.T) {
	fx, _ := newRenewFixture(t, 0, nil)
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	fx.registerNode(fx.nodeClient(cert), id)
	ctx := context.Background()
	if _, err := fx.st.RemoveNode(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	_, csr, _ := newCSR(t)
	if _, err := fx.apiClient(cert).Renew(ctx, id, csr); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Fatalf("renewal by a removed node: %v, want ErrNodeRemoved", err)
	}
	if _, err := fx.st.NodeCertExpiry(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a removed node keeps a certificate record: %v", err)
	}
	// The store refuses the record too, for a request that raced the removal.
	if err := fx.st.RecordNodeCert(ctx, id, time.Now().Add(time.Hour)); !errors.Is(err, store.ErrNodeRemoved) {
		t.Fatalf("RecordNodeCert for a removed node: %v", err)
	}
}

// A certificate that has expired by the server's clock is refused on every
// route, renewal included, even on a connection opened while it was valid.
func TestExpiredCertIsRefusedByServerClock(t *testing.T) {
	clk := &fakeClock{}
	fx, _ := newRenewFixture(t, time.Hour, clk.now)
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	c := fx.apiClient(cert)
	ctx := context.Background()
	if err := c.Register(ctx, id, nodeapi.Register{Name: id, CPU: 1, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour)
	_, csr, _ := newCSR(t)
	if _, err := c.Renew(ctx, id, csr); !errors.Is(err, nodeapi.ErrCertInvalid) {
		t.Fatalf("renewal with an expired certificate: %v, want ErrCertInvalid", err)
	}
	if _, err := c.AssignmentsForNode(ctx, id); !errors.Is(err, nodeapi.ErrCertInvalid) {
		t.Fatalf("assignments with an expired certificate: %v, want ErrCertInvalid", err)
	}
}

// A certificate that has expired by the real clock fails the handshake.
func TestExpiredCertFailsHandshake(t *testing.T) {
	fx, _ := newRenewFixture(t, 0, nil)
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	der, err := fx.ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-old", Now: time.Now().Add(-2 * time.Hour), Lifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, fx.ca)
	if err != nil {
		t.Fatal(err)
	}
	_, req, _ := newCSR(t)
	if _, err := fx.apiClient(cert).Renew(context.Background(), "node-old", req); err == nil {
		t.Fatal("an expired certificate renewed")
	}
}

// The server renews its own certificate once two thirds of its lifetime
// have passed, and new handshakes get the new one.
func TestServerCertRenewsItself(t *testing.T) {
	fx, c := newRenewFixture(t, 6*time.Second, nil)
	first := c.ServerCert().Leaf
	// A client certificate that outlasts the test.
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	der, err := fx.ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-client"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, fx.ca)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pki.ClientTLSConfig(fx.ca, cert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	presented := func() *x509.Certificate {
		conn, err := tls.Dial("tcp", fx.nodeAddr, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}
	if got := presented(); got.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Fatal("the server does not present its first certificate")
	}
	deadline := time.Now().Add(10 * time.Second)
	for c.ServerCert().Leaf.SerialNumber.Cmp(first.SerialNumber) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the server certificate was not renewed within 10 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	renewed := c.ServerCert().Leaf
	if got := presented(); got.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Fatalf("a new handshake still gets the first certificate (renewed serial %s)", renewed.SerialNumber)
	}
	if id, err := pki.NodeIDFromCert(renewed); err != nil || id != "node-server" || len(renewed.IPAddresses) != 1 {
		t.Fatalf("renewed server certificate names %q (%v), IPs %v", id, err, renewed.IPAddresses)
	}
}

// A renewal with a body that is not a CSR is refused with 400.
func TestRenewRejectsBadCSR(t *testing.T) {
	fx, _ := newRenewFixture(t, 0, nil)
	id, cert := fx.enroll(fx.issue(pki.DefaultTokenTTL))
	code, body := fx.post(fx.nodeClient(cert), fx.nodeURL(nodeapi.RenewPath(id)), nodeapi.RenewRequest{CSR: []byte("nope")})
	var e nodeapi.ErrorBody
	_ = json.Unmarshal(body, &e)
	if code != http.StatusBadRequest {
		t.Fatalf("bad CSR: %d %q, want 400", code, e.Error)
	}
}

// lockedBuffer is a log destination tests can read while the server writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The server logs a warning for a worker whose certificate is past its
// renewal time, at start and then once a day, and none for a worker that
// renewed or for its own certificate.
func TestServerWarnsOnceADayForANodeThatIsNotRenewing(t *testing.T) {
	ctx := context.Background()
	clock := &fakeClock{}
	dir := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	day := 24 * time.Hour
	for id, issued := range map[string]time.Time{"node-old": clock.now().Add(-245 * day), "node-new": clock.now().Add(-day)} {
		if err := st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 1, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordNodeCertValidity(ctx, id, issued, issued.Add(365*day)); err != nil {
			t.Fatal(err)
		}
	}
	var logs lockedBuffer
	c, err := Listen(Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()), Advertise: "127.0.0.1", Port: 0,
		LocalNodeID: "node-server", Logger: slog.New(slog.NewTextHandler(&logs, nil)), Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	warnings := func() int { return strings.Count(logs.String(), "has not renewed its certificate") }
	if n := warnings(); n != 1 || !strings.Contains(logs.String(), "node=node-old") || strings.Contains(logs.String(), "node=node-new") {
		t.Fatalf("%d warnings at start, want one for node-old:\n%s", n, logs.String())
	}
	clock.advance(time.Hour)
	c.certs.check()
	if n := warnings(); n != 1 {
		t.Fatalf("%d warnings an hour later, want still one:\n%s", n, logs.String())
	}
	clock.advance(23 * time.Hour)
	c.certs.check()
	if n := warnings(); n != 2 {
		t.Fatalf("%d warnings a day later, want two:\n%s", n, logs.String())
	}
}
