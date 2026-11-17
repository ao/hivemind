package cluster

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// joinedClient joins fx and returns a node API client for the new node, its
// ID and its certificate.
func joinedClient(t *testing.T, fx *fixture, opts ...nodeapi.ClientOption) (*nodeapi.Client, string, tls.Certificate) {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := nodeapi.Join(context.Background(), fx.joinAddr, fx.issue(time.Hour), csr)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	tlsCert, err := pki.NewTLSCertificate(certDER(t, reply.CertPEM), key, fx.ca)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pki.ClientTLSConfig(fx.ca, tlsCert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := nodeapi.NewClient("https://"+fx.nodeAddr, cfg, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c, reply.NodeID, tlsCert
}

// A worker's version reaches the store at registration, and again from a
// lease or a report once it changes, as after an upgrade without a new
// registration. Every node API reply carries the server's version.
func TestNodeVersionsOverTheNodeAPI(t *testing.T) {
	fx := newFixtureWith(t, func(c *Config) { c.Version = "v2.2.0" })
	ctx := context.Background()
	version := func(id string) string {
		t.Helper()
		n, err := fx.st.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Version
	}

	c, id, cert := joinedClient(t, fx, nodeapi.WithVersion("v2.2.0-rc.1"))
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w1", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if v := version(id); v != "v2.2.0-rc.1" {
		t.Fatalf("version after register: %q", v)
	}

	// The same node, upgraded: a lease and a report carry the new version.
	c2, err := nodeapi.NewClient("https://"+fx.nodeAddr, mustClientTLS(t, fx, cert), nodeapi.WithVersion("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c2.RenewLease(ctx, id); err != nil {
		t.Fatal(err)
	}
	if v := version(id); v != "v2.2.0" {
		t.Fatalf("version after a lease: %q", v)
	}
	// One that sends no version, or one that is not a version, reads as none.
	c3, err := nodeapi.NewClient("https://"+fx.nodeAddr, mustClientTLS(t, fx, cert), nodeapi.WithVersion("v2 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c3.ReportStatus(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if v := version(id); v != "" {
		t.Fatalf("version after a report with no version: %q", v)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+fx.nodeAddr+nodeapi.StatusPath(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fx.nodeClient(cert).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get(nodeapi.VersionHeader); got != "v2.2.0" {
		t.Fatalf("reply %s: %q, want the server's v2.2.0", nodeapi.VersionHeader, got)
	}
}

func mustClientTLS(t *testing.T, fx *fixture, cert tls.Certificate) *tls.Config {
	t.Helper()
	cfg, err := pki.ClientTLSConfig(fx.ca, cert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCleanVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v2.2.0": "v2.2.0", "v2.2.0-3-gabc123-dirty": "v2.2.0-3-gabc123-dirty", "dev": "dev", "1.0+build_7": "1.0+build_7",
		"": "", "v2 2": "", "v2\n": "", "<b>": "", "v2.2.0\x00": "",
		"v1234567890123456789012345678901234567890123456789012345678901234": "",
	} {
		if got := nodeapi.CleanVersion(in); got != want {
			t.Errorf("CleanVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// The node API marks its 404 for a node it does not know. A route it lacks
// gets http.ServeMux's plain 404 without the mark, which a worker reads as
// nodeapi.ErrNoRoute.
func TestNotRegisteredIsMarked(t *testing.T) {
	fx := newFixtureWith(t, func(c *Config) { c.Version = "v2.2.0" })
	ctx := context.Background()
	c, id, cert := joinedClient(t, fx)
	if err := c.ReportStatus(ctx, id, nil); !errors.Is(err, nodeapi.ErrNotRegistered) {
		t.Fatalf("report before register: %v", err)
	}
	if err := c.RenewLease(ctx, id); !errors.Is(err, nodeapi.ErrNotRegistered) {
		t.Fatalf("lease before register: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "https://"+fx.nodeAddr+nodeapi.Prefix+"/nodes/"+id+"/no-such-route", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fx.nodeClient(cert).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || resp.Header.Get(nodeapi.ErrorHeader) != "" || strings.HasPrefix(string(b), "{") {
		t.Fatalf("a route the server lacks: %d %v %q", resp.StatusCode, resp.Header, b)
	}
}

// A worker's registration carries the server address it dials, which the
// store keeps for GET /v1/nodes; one without it, from an earlier build, is
// taken, and leaves none. A malformed one is refused.
func TestRegisterRecordsWhatTheNodeDials(t *testing.T) {
	fx := newFixture(t, nil)
	ctx := context.Background()
	c, id, _ := joinedClient(t, fx)
	dials := func() string {
		t.Helper()
		n, err := fx.st.GetNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return n.Dials
	}
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w1", CPU: 2, Memory: 1 << 30, Server: "hm.example.internal:4484"}); err != nil {
		t.Fatal(err)
	}
	if got := dials(); got != "hm.example.internal:4484" {
		t.Fatalf("dials after register: %q", got)
	}
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w1", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if got := dials(); got != "" {
		t.Fatalf("dials after a register without it: %q", got)
	}
	var refused *nodeapi.RefusedError
	if err := c.Register(ctx, id, nodeapi.Register{Name: "w1", Server: "hm.example.internal"}); !errors.As(err, &refused) || refused.Code != http.StatusBadRequest {
		t.Fatalf("a server address without a port: %v", err)
	}
}
