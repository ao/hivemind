package router

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// testIssuer is a CA that certmagic can use as its issuer: it signs every
// CSR at once, for a day, and records the names it signed for.
type testIssuer struct {
	ca  *x509.Certificate
	key crypto.Signer

	mu     sync.Mutex
	issued []string
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test issuer"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testIssuer{ca: ca, key: key}
}

func (i *testIssuer) IssuerKey() string { return "test-issuer" }

func (i *testIssuer) Issue(_ context.Context, csr *x509.CertificateRequest) (*certmagic.IssuedCertificate, error) {
	i.mu.Lock()
	i.issued = append(i.issued, csr.DNSNames...)
	i.mu.Unlock()
	der, err := i.sign(csr.PublicKey, csr.DNSNames, time.Now().Add(-time.Minute), time.Now().Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	return &certmagic.IssuedCertificate{Certificate: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

func (i *testIssuer) sign(pub any, names []string, notBefore, notAfter time.Time) ([]byte, error) {
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), DNSNames: names,
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	return x509.CreateCertificate(rand.Reader, tmpl, i.ca, pub, i.key)
}

func (i *testIssuer) names() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.issued)
}

// store puts a certificate for host that runs from notBefore to notAfter in
// storage, as another router would have obtained it from this issuer.
func (i *testIssuer) store(t *testing.T, ts certmagic.Storage, host string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := i.sign(&key.PublicKey, []string{host}, notBefore, notAfter)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string][]byte{
		certmagic.StorageKeys.SiteCert(i.IssuerKey(), host):       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		certmagic.StorageKeys.SitePrivateKey(i.IssuerKey(), host): pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		certmagic.StorageKeys.SiteMeta(i.IssuerKey(), host):       []byte(`{"sans":["` + host + `"],"issuer_data":null}`),
	} {
		if err := ts.Store(context.Background(), k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// A router that starts with a certificate already in its renewal window
// renews it at once. certmagic's own check runs every 10 minutes, and a
// certificate the router loads before it serves TLS is already managed, so
// the management that starts for the host skips its renewal check. In the
// third soak, routers restarted with such certificates logged no renewal
// until 10 minutes later. A certificate that is not due is left alone.
func TestStartRenewsACertificateInItsRenewalWindow(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts, err := tlsstore.New(tlsstore.StoreBackend{St: st, NodeID: "node-a"}, tlsstore.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	const due, fresh = "due.example.com", "fresh.example.com"
	routes := func(context.Context) (Table, error) { return Table{due: {}, fresh: {}}, nil }
	rt, err := New(Config{Addr: "127.0.0.1", Routes: routes, Storage: ts, Email: "ops@example.com",
		CA: "https://acme.invalid/dir", Logger: log, Metrics: NewMetrics(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Management of new hosts is a no-op here, as in the other tests: the
	// renewal must come from the start itself.
	rt.manage = func(context.Context, []string) error { return nil }
	iss := newTestIssuer(t)
	rt.magic.Issuers = []certmagic.Issuer{iss}
	// A one-hour certificate with 10 minutes left, well inside the last
	// third of its life; and a day-long one just issued.
	now := time.Now()
	iss.store(t, ts, due, now.Add(-50*time.Minute), now.Add(10*time.Minute))
	iss.store(t, ts, fresh, now.Add(-time.Minute), now.Add(24*time.Hour))

	go func() { _ = rt.Serve() }()
	t.Cleanup(func() { _ = rt.Shutdown() })

	pool := x509.NewCertPool()
	pool.AddCert(iss.ca)
	served := func(host string) *x509.Certificate {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", rt.HTTPSAddr(),
			&tls.Config{RootCAs: pool, ServerName: host})
		if err != nil {
			t.Fatalf("handshake for %s: %v", host, err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0]
	}
	deadline := time.Now().Add(20 * time.Second)
	for served(due).NotAfter.Before(now.Add(time.Hour)) {
		if time.Now().After(deadline) {
			t.Fatalf("20 s after the start the router still serves the old certificate for %s; issued for %v", due, iss.names())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := iss.names(); !slices.Equal(got, []string{due}) {
		t.Fatalf("issued for %v, want %s alone", got, due)
	}
	if got := served(fresh).NotAfter; got.Before(now.Add(23 * time.Hour)) {
		t.Fatalf("%s served until %s", fresh, got)
	}
}

// A router whose table cannot be read as it starts, as a server's while no
// server leads during an election, reads it again until it can, for up to
// warmTimeout, before it takes TLS connections: the first handshake gets the
// certificate the storage has. In the eighth soak a restarted leader's
// router read once, during the election its stop caused, and failed every
// handshake until its poll read the table 1.9 s later.
func TestStartWaitsForATableThatComesSoon(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts, err := tlsstore.New(tlsstore.StoreBackend{St: st, NodeID: "node-a"}, tlsstore.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	const host = "www.example.com"
	var mu sync.Mutex
	reads := 0
	routes := func(context.Context) (Table, error) {
		mu.Lock()
		defer mu.Unlock()
		if reads++; reads <= 2 {
			return nil, errors.New("no leader: this server reaches 1 of the 3 servers, and a write needs 2")
		}
		return Table{host: {}}, nil
	}
	rt, err := New(Config{Addr: "127.0.0.1", Routes: routes, Storage: ts, Email: "ops@example.com",
		CA: "https://acme.invalid/dir", Logger: log, Metrics: NewMetrics(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rt.manage = func(context.Context, []string) error { return nil }
	iss := newTestIssuer(t)
	rt.magic.Issuers = []certmagic.Issuer{iss}
	iss.store(t, ts, host, time.Now().Add(-time.Minute), time.Now().Add(24*time.Hour))

	go func() { _ = rt.Serve() }()
	t.Cleanup(func() { _ = rt.Shutdown() })
	time.Sleep(100 * time.Millisecond)
	pool := x509.NewCertPool()
	pool.AddCert(iss.ca)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", rt.HTTPSAddr(), &tls.Config{RootCAs: pool, ServerName: host})
	if err != nil {
		t.Fatalf("a handshake 100 ms after the start: %v", err)
	}
	conn.Close()
	mu.Lock()
	defer mu.Unlock()
	if reads < 3 {
		t.Fatalf("%d reads of the table", reads)
	}
}
