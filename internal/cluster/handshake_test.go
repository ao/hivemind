package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// handshakeFixture is a node listener on loopback whose log a test reads,
// on a clock it moves, with nodeAt as its lookup of a node by address:
// loopback is no address a node can register.
type handshakeFixture struct {
	*fixture
	logs  *lockedBuffer
	clock *fakeClock
}

func newHandshakeFixture(t *testing.T, nodeAt func(context.Context, netip.Addr) (string, string, bool)) *handshakeFixture {
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
	logs, clock := &lockedBuffer{}, &fakeClock{}
	tokens := pki.NewTokens(st.JoinTokens())
	c, err := Listen(Config{Store: st, CA: ca, Tokens: tokens, Advertise: "127.0.0.1", LocalNodeID: "node-server",
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Now: clock.now})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	c.refused.nodeAt = nodeAt
	go func() { _ = c.Serve() }()
	t.Cleanup(func() {
		_ = c.Shutdown()
		st.Close()
	})
	return &handshakeFixture{fixture: &fixture{t: t, st: st, ca: ca, tokens: tokens, nodeAddr: c.NodeAddr(), joinAddr: c.JoinAddr()},
		logs: logs, clock: clock}
}

// certFrom is a node certificate for id from ca, with ca in its chain, as a
// worker presents its own.
func certFrom(t *testing.T, ca *pki.CA, id string) tls.Certificate {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, ca)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// waitLog waits for the server's log to have n lines with want.
func (hx *handshakeFixture) waitLog(want string, n int) string {
	hx.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := hx.logs.String()
		if strings.Count(l, want) >= n {
			return l
		}
		if time.Now().After(deadline) {
			hx.t.Fatalf("want %d lines with %q, log:\n%s", n, want, l)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A worker whose certificate is from a CA the cluster no longer trusts,
// as after finish --force for a worker that trusted the new CA and had not
// renewed, is still refused, and the server names it once a minute, from
// the refused certificate, unverified, with the CA's fingerprint and what
// to do. In the fifth soak a cut-off worker showed only as an unnamed
// "http: TLS handshake error" at info every 5 s.
func TestNodeListenerNamesACertificateFromAnUntrustedCA(t *testing.T) {
	hx := newHandshakeFixture(t, nil)
	old, err := pki.CreateCA(filepath.Join(t.TempDir(), "old"))
	if err != nil {
		t.Fatal(err)
	}
	client := hx.nodeClient(certFrom(t, old, "node-cutoff"))
	for range 3 {
		resp, err := client.Get(hx.nodeURL(nodeapi.AssignmentsPath("node-cutoff")))
		if err == nil {
			resp.Body.Close()
			t.Fatal("a certificate from a CA the cluster does not trust was accepted")
		}
		client.CloseIdleConnections()
	}
	warn := `level=WARN msg="a node presented a certificate from a CA this cluster does not trust, and was refused`
	hx.waitLog(warn, 1)
	time.Sleep(100 * time.Millisecond) // the other handshakes' lines, if any
	l := hx.logs.String()
	if n := strings.Count(l, warn); n != 1 {
		t.Fatalf("%d warn lines for three refused handshakes in a second, want one:\n%s", n, l)
	}
	if strings.Contains(l, "TLS handshake error") {
		t.Fatalf("the refused handshakes were logged at info too:\n%s", l)
	}
	for _, want := range []string{"likely cut off by a rotation of the cluster CA", "read from the refused certificate, which is not verified",
		"hivemind node remove <id>", "join the machine again with an empty data directory", "node=node-cutoff", "node_id_verified=false",
		"ca=" + shortFP(old.Fingerprint()), "trusted=" + shortFP(hx.ca.Fingerprint())} {
		if !strings.Contains(l, want) {
			t.Errorf("the line does not say %q:\n%s", want, l)
		}
	}

	// A minute on, it is named again.
	hx.clock.advance(61 * time.Second)
	if resp, err := client.Get(hx.nodeURL(nodeapi.AssignmentsPath("node-cutoff"))); err == nil {
		resp.Body.Close()
		t.Fatal("accepted a minute on")
	}
	hx.waitLog(warn, 2)

	// A node of the cluster still gets through the listener.
	id, cert := hx.enroll(hx.issue(time.Minute))
	resp, err := hx.nodeClient(cert).Get(hx.nodeURL(nodeapi.AssignmentsPath(id)))
	if err != nil {
		t.Fatalf("a node of the cluster: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a node of the cluster: status %d", resp.StatusCode)
	}
}

// A worker that does not trust the CA of the server's certificate, as one
// cut off by finish --force that trusts the old CA only, ends the handshake
// before it sends its own certificate. The server names the node that
// registered the client's address, once a minute; a client at an address
// no node registered is logged at info, as before.
func TestNodeListenerNamesANodeThatRefusesItsCertificate(t *testing.T) {
	var mu sync.Mutex
	known, lookups := netip.MustParseAddr("127.0.0.1"), 0
	hx := newHandshakeFixture(t, func(_ context.Context, ip netip.Addr) (string, string, bool) {
		mu.Lock()
		defer mu.Unlock()
		lookups++
		if ip == known {
			return "node-cutoff", "box", true
		}
		return "", "", false
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return lookups
	}
	old, err := pki.CreateCA(filepath.Join(t.TempDir(), "old"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(old.Cert)
	cert := certFrom(t, old, "node-cutoff")
	dial := func() {
		t.Helper()
		c, err := tls.Dial("tcp", hx.nodeAddr, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{cert}})
		if err == nil {
			c.Close()
			t.Fatal("the client accepted a server certificate from a CA it does not trust")
		}
	}
	for range 3 {
		dial()
	}
	warn := `level=WARN msg="a node refused this server's certificate, so it does not trust the CA of the certificate`
	hx.waitLog(warn, 1)
	time.Sleep(100 * time.Millisecond)
	l := hx.logs.String()
	if n := strings.Count(l, warn); n != 1 || strings.Contains(l, "TLS handshake error") {
		t.Fatalf("three refusals in a second: %d warn lines, want one, and none at info:\n%s", n, l)
	}
	for _, want := range []string{"likely cut off by a rotation of the cluster CA", "known by its address alone", "hivemind node remove <id>",
		"node=node-cutoff", "name=box", "remote=127.0.0.1:"} {
		if !strings.Contains(l, want) {
			t.Errorf("the line does not say %q:\n%s", want, l)
		}
	}
	if count() != 1 {
		t.Errorf("%d lookups of the address for three refusals, want one", count())
	}

	// An address that is no node's: info, as before, and one lookup a minute.
	mu.Lock()
	known = netip.Addr{}
	mu.Unlock()
	hx.clock.advance(61 * time.Second)
	for range 2 {
		dial()
	}
	hx.waitLog("TLS handshake error on the node listener", 2)
	if n := strings.Count(hx.logs.String(), warn); n != 1 || count() != 2 {
		t.Fatalf("an address that is no node's: %d warn lines, %d lookups; want 1 and 2:\n%s", n, count(), hx.logs.String())
	}
}

// The warn lines are bounded over every key, so that certificates with
// made-up node IDs cannot fill the log faster than the info lines did.
func TestRefusedLogIsBounded(t *testing.T) {
	logs, clock := &lockedBuffer{}, &fakeClock{}
	r := &refusedLog{log: slog.New(slog.NewTextHandler(logs, nil)), now: clock.now}
	allowed := 0
	for i := range refusedBurst + 5 {
		if r.allow(fmt.Sprintf("node:node-%012d", i)) {
			allowed++
		}
	}
	if allowed != refusedBurst {
		t.Fatalf("%d keys allowed in a minute, want %d", allowed, refusedBurst)
	}
	clock.advance(time.Minute)
	if !r.allow("node:node-000000000099") || r.allow("node:node-000000000099") {
		t.Fatal("a minute on: want one more line, then none for that key")
	}
	if !refusedServerCert(&net.OpError{Op: "remote error", Err: fakeAlert("tls: bad certificate")}) ||
		!refusedServerCert(&net.OpError{Op: "remote error", Err: fakeAlert("tls: unknown certificate authority")}) ||
		refusedServerCert(&net.OpError{Op: "remote error", Err: fakeAlert("tls: bad record MAC")}) ||
		refusedServerCert(&net.OpError{Op: "local error", Err: fakeAlert("tls: bad certificate")}) {
		t.Fatal("refusedServerCert")
	}
}

type fakeAlert string

func (a fakeAlert) Error() string { return string(a) }
