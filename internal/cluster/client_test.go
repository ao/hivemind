package cluster

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// The worker's side, against a real listener: join with nodeapi.Join, then
// use nodeapi.Client for every route.
func TestWorkerClientRoundTrip(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := nodeapi.Join(ctx, fx.joinAddr, fx.issue(time.Hour), csr)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	der := certDER(t, reply.CertPEM)
	tlsCert, err := pki.NewTLSCertificate(der, key, fx.ca)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pki.ClientTLSConfig(fx.ca, tlsCert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := nodeapi.NewClient("https://"+fx.nodeAddr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := reply.NodeID

	// A report before registration is refused, so the worker knows to register.
	if err := c.ReportStatus(ctx, id, nil); !errors.Is(err, nodeapi.ErrNotRegistered) {
		t.Fatalf("report before register: %v, want ErrNotRegistered", err)
	}
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w1", CPU: 4, Memory: 1 << 30}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := c.AssignmentsForNode(ctx, id); err != nil {
		t.Fatalf("assignments: %v", err)
	}
	sts := []store.Status{{Name: "hivemind-web-0-g1", App: "web", Replica: 0, Generation: 1, State: "running",
		Ready: true, RestartCount: 2, Health: "healthy", NextRestart: time.Unix(1_700_000_000, 0).UTC()}}
	if err := c.ReportStatus(ctx, id, sts); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, err := c.StatusForNode(ctx, id)
	if err != nil {
		t.Fatalf("status for node: %v", err)
	}
	if len(got) != 1 || got[0].RestartCount != 2 || got[0].NodeID != id || !got[0].Ready || got[0].Health != "healthy" {
		t.Fatalf("status for node: %+v", got)
	}
}

// A token that pins another cluster's CA is refused by the client's pinned TLS
// check, before any request is sent.
func TestJoinPinsTheTokensCA(t *testing.T) {
	fx := newFixture(t, nil)
	other, err := pki.CreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	ost, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ost.Close()
	foreign, _, err := pki.NewTokens(ost.JoinTokens()).Issue(other.Fingerprint(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeapi.Join(context.Background(), fx.joinAddr, foreign, csr); err == nil {
		t.Fatal("joined with a token that pins another CA")
	}
}

// A join address needs a port: it is the join listener's host:port.
func TestJoinNeedsHostPort(t *testing.T) {
	fx := newFixture(t, nil)
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodeapi.Join(context.Background(), "127.0.0.1", fx.issue(time.Hour), csr); err == nil {
		t.Fatal("a join address without a port was accepted")
	}
}

// The node certificate is only good for the names it covers. A client that
// dials another name is refused by the handshake.
func TestClientRefusesAServerNameTheCertificateDoesNotCover(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	cfg, err := pki.ClientTLSConfig(fx.ca, cert, "node-server.example")
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	if _, err := c.Get(fx.nodeURL(nodeapi.AssignmentsPath(id))); err == nil {
		t.Fatal("the server certificate was accepted for a name it does not cover")
	}
}

// certDER returns the DER bytes of the first PEM certificate in s.
func certDER(t *testing.T, s string) []byte {
	t.Helper()
	b, _ := pem.Decode([]byte(s))
	if b == nil || b.Type != "CERTIFICATE" {
		t.Fatal("no certificate PEM")
	}
	return b.Bytes
}
