package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// fixture is a server with both listeners on loopback, and its store.
type fixture struct {
	t        *testing.T
	st       *store.Store
	ca       *pki.CA
	tokens   *pki.Tokens
	nodeAddr string
	joinAddr string
}

func newFixture(t *testing.T, limiter *FailLimiter) *fixture {
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
		LocalNodeID: "node-server", Limiter: limiter, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	go func() { _ = c.Serve() }()
	t.Cleanup(func() {
		_ = c.Shutdown()
		st.Close()
	})
	return &fixture{t: t, st: st, ca: ca, tokens: tokens, nodeAddr: c.NodeAddr(), joinAddr: c.JoinAddr()}
}

// issue makes a token for this cluster.
func (fx *fixture) issue(ttl time.Duration) string {
	fx.t.Helper()
	tok, _, err := fx.tokens.Issue(fx.ca.Fingerprint(), ttl)
	if err != nil {
		fx.t.Fatal(err)
	}
	return tok
}

func fingerprintOf(t *testing.T, tok string) [32]byte {
	t.Helper()
	p, err := pki.ParseJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	return p.Fingerprint
}

// joinClient is an HTTP client for the join listener, pinned to the CA with
// fingerprint pin. A joining node pins the CA named by its token.
func (fx *fixture) joinClient(pin [32]byte) *http.Client {
	fx.t.Helper()
	cfg, err := pki.JoinClientTLSConfig(pin, "127.0.0.1")
	if err != nil {
		fx.t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}
}

// nodeClient is an HTTP client for the node listener that presents cert.
func (fx *fixture) nodeClient(cert tls.Certificate) *http.Client {
	fx.t.Helper()
	cfg, err := pki.ClientTLSConfig(fx.ca, cert, "127.0.0.1")
	if err != nil {
		fx.t.Fatal(err)
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 10 * time.Second}
}

// enroll joins with token, which must succeed.
func (fx *fixture) enroll(token string) (string, tls.Certificate) {
	fx.t.Helper()
	id, cert, code, msg := fx.tryJoin(token)
	if code != http.StatusOK {
		fx.t.Fatalf("join: status %d: %s", code, msg)
	}
	return id, cert
}

// tryJoin joins with the CA the token names.
func (fx *fixture) tryJoin(token string) (string, tls.Certificate, int, string) {
	fx.t.Helper()
	return fx.tryJoinPinned(fingerprintOf(fx.t, token), token)
}

// tryJoinPinned joins with a client pinned to pin, sending token. It returns
// the node ID and certificate on success, and the status and error text
// otherwise.
func (fx *fixture) tryJoinPinned(pin [32]byte, token string) (string, tls.Certificate, int, string) {
	fx.t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		fx.t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		fx.t.Fatal(err)
	}
	code, body := fx.post(fx.joinClient(pin), "https://"+fx.joinAddr+nodeapi.JoinPath,
		nodeapi.JoinRequest{Token: token, CSR: csr})
	if code != http.StatusOK {
		var e nodeapi.ErrorBody
		_ = json.Unmarshal(body, &e)
		return "", tls.Certificate{}, code, e.Error
	}
	var reply nodeapi.JoinReply
	if err := json.Unmarshal(body, &reply); err != nil {
		fx.t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(reply.CertPEM))
	if blk == nil {
		fx.t.Fatal("join reply has no certificate PEM")
	}
	cert, err := pki.NewTLSCertificate(blk.Bytes, key, fx.ca)
	if err != nil {
		fx.t.Fatal(err)
	}
	return reply.NodeID, cert, code, ""
}

// post sends v as JSON and returns the status and the reply body.
func (fx *fixture) post(c *http.Client, u string, v any) (int, []byte) {
	fx.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		fx.t.Fatal(err)
	}
	return fx.send(c, http.MethodPost, u, b)
}

// send makes one request and returns the status and the reply body.
func (fx *fixture) send(c *http.Client, method, u string, body []byte) (int, []byte) {
	fx.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		fx.t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		fx.t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b
}

func (fx *fixture) nodeURL(path string) string { return "https://" + fx.nodeAddr + path }

// registerNode gives a joined node its capacity, which creates its row.
func (fx *fixture) registerNode(c *http.Client, id string) {
	fx.t.Helper()
	code, body := fx.post(c, fx.nodeURL(nodeapi.RegisterPath(id)), nodeapi.Register{Name: id, CPU: 2, Memory: 1 << 30})
	if code != http.StatusNoContent {
		fx.t.Fatalf("register %s: %d %s", id, code, body)
	}
}

// --- join ------------------------------------------------------------------

func TestJoinEnrollsANodeThatCanUseItsRoutes(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	if err := pki.ValidateNodeID(id); err != nil {
		t.Fatalf("server chose node ID %q: %v", id, err)
	}
	c := fx.nodeClient(cert)
	fx.registerNode(c, id)
	if code, body := fx.send(c, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(id)), nil); code != http.StatusOK {
		t.Fatalf("assignments: %d %s", code, body)
	}
	if code, body := fx.post(c, fx.nodeURL(nodeapi.StatusPath(id)), nodeapi.StatusReport{}); code != http.StatusNoContent {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, body := fx.send(c, http.MethodGet, fx.nodeURL(nodeapi.StatusPath(id)), nil); code != http.StatusOK {
		t.Fatalf("get status: %d %s", code, body)
	}
}

func TestTokenUsedTwiceIsRefused(t *testing.T) {
	fx := newFixture(t, nil)
	tok := fx.issue(time.Hour)
	fx.enroll(tok)
	_, _, code, msg := fx.tryJoin(tok)
	if code != http.StatusUnauthorized || msg != errJoinRefused {
		t.Fatalf("second use: %d %q, want 401 %q", code, msg, errJoinRefused)
	}
}

func TestExpiredTokenIsRefused(t *testing.T) {
	fx := newFixture(t, nil)
	tok := fx.issue(time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if _, _, code, _ := fx.tryJoin(tok); code != http.StatusUnauthorized {
		t.Fatalf("expired token: status %d, want 401", code)
	}
}

// A token whose text names another CA is refused. The client is pinned to
// this cluster's CA, so the server's check is the one that refuses.
func TestTokenNamingAnotherCAIsRefusedAndTheRealTokenSurvives(t *testing.T) {
	fx := newFixture(t, nil)
	real := fx.issue(time.Hour)
	parsed, err := pki.ParseJoinToken(real)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Fingerprint[0] ^= 0xff
	forged := parsed.String()
	if _, _, code, _ := fx.tryJoinPinned(fx.ca.Fingerprint(), forged); code != http.StatusUnauthorized {
		t.Fatalf("forged fingerprint: status %d, want 401", code)
	}
	fx.enroll(real)
}

// A token minted by another cluster's store is unknown here.
func TestTokenFromAnotherClusterIsRefused(t *testing.T) {
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
	if _, _, code, _ := fx.tryJoinPinned(fx.ca.Fingerprint(), foreign); code != http.StatusUnauthorized {
		t.Fatalf("token from another cluster: status %d, want 401", code)
	}
}

// A refused CSR does not use up the token.
func TestBadCSRLeavesTheTokenUsable(t *testing.T) {
	fx := newFixture(t, nil)
	tok := fx.issue(time.Hour)
	code, body := fx.post(fx.joinClient(fingerprintOf(t, tok)), "https://"+fx.joinAddr+nodeapi.JoinPath,
		nodeapi.JoinRequest{Token: tok, CSR: []byte("not a CSR")})
	if code != http.StatusBadRequest {
		t.Fatalf("bad CSR: %d %s, want 400", code, body)
	}
	fx.enroll(tok)
}

func TestJoinBadBodies(t *testing.T) {
	fx := newFixture(t, nil)
	tok := fx.issue(time.Hour)
	c := fx.joinClient(fingerprintOf(t, tok))
	u := "https://" + fx.joinAddr + nodeapi.JoinPath
	cases := map[string]struct {
		body string
		want int
	}{
		"not json":      {`nope`, http.StatusBadRequest},
		"unknown field": {`{"token":"x","csr":"","x":1}`, http.StatusBadRequest},
		"trailing":      {`{"token":"x"}{}`, http.StatusBadRequest},
		"too large":     {`{"token":"` + strings.Repeat("a", nodeapi.MaxJoinBytes) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for name, tc := range cases {
		code, _ := fx.send(c, http.MethodPost, u, []byte(tc.body))
		if code != tc.want {
			t.Errorf("%s: status %d, want %d", name, code, tc.want)
		}
	}
	fx.enroll(tok) // none of the bad bodies used the token
}

// The join listener serves POST /join and nothing else, so no node route is
// reachable through it, even by a caller who presents a node certificate.
func TestJoinListenerServesNoNodeRoutes(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	cfg, err := pki.JoinClientTLSConfig(fx.ca.Fingerprint(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Certificates = []tls.Certificate{cert}
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, nodeapi.AssignmentsPath(id)},
		{http.MethodGet, nodeapi.StatusPath(id)},
		{http.MethodPost, nodeapi.RegisterPath(id)},
		{http.MethodPost, nodeapi.StatusPath(id)},
	} {
		code, _ := fx.send(c, rt.method, "https://"+fx.joinAddr+rt.path, nil)
		if code != http.StatusNotFound {
			t.Errorf("%s %s on the join listener: %d, want 404", rt.method, rt.path, code)
		}
	}
}

// The node listener has no join route, even for an authenticated node.
func TestNodeListenerServesNoJoinRoute(t *testing.T) {
	fx := newFixture(t, nil)
	_, cert := fx.enroll(fx.issue(time.Hour))
	if code, _ := fx.post(fx.nodeClient(cert), fx.nodeURL(nodeapi.JoinPath), nodeapi.JoinRequest{}); code != http.StatusNotFound {
		t.Fatalf("POST /join on the node listener: %d, want 404", code)
	}
}

// --- node listener: mutual TLS ---------------------------------------------

func TestNodeListenerRefusesMissingClientCertificate(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := &tls.Config{RootCAs: caPool(fx.ca), ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13}
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
	if _, err := c.Get(fx.nodeURL(nodeapi.AssignmentsPath("node-x"))); err == nil {
		t.Fatal("request without a client certificate succeeded")
	}
}

func TestNodeListenerRefusesCertificateFromAnotherCA(t *testing.T) {
	fx := newFixture(t, nil)
	other, err := pki.CreateCA(filepath.Join(t.TempDir(), "ca"))
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
	der, err := other.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-evil"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.nodeClient(cert).Get(fx.nodeURL(nodeapi.AssignmentsPath("node-evil"))); err == nil {
		t.Fatal("a certificate from another CA was accepted")
	}
}

// A certificate signed by the cluster CA that names no node has a valid chain,
// and the handshake still refuses it.
func TestNodeListenerRefusesCertificateWithoutNodeID(t *testing.T) {
	fx := newFixture(t, nil)
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "no-id"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, fx.ca.Cert, &key.PublicKey, fx.ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, fx.ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.nodeClient(cert).Get(fx.nodeURL(nodeapi.AssignmentsPath("node-x"))); err == nil {
		t.Fatal("a certificate with no node ID was accepted")
	}
}

// A joined node can read and report for itself, and for no other node.
func TestNodeCannotActForAnotherNode(t *testing.T) {
	fx := newFixture(t, nil)
	idA, certA := fx.enroll(fx.issue(time.Hour))
	idB, certB := fx.enroll(fx.issue(time.Hour))
	a := fx.nodeClient(certA)
	b := fx.nodeClient(certB)
	fx.registerNode(a, idA)
	fx.registerNode(b, idB)

	for _, p := range []string{nodeapi.AssignmentsPath(idB), nodeapi.StatusPath(idB)} {
		if code, _ := fx.send(a, http.MethodGet, fx.nodeURL(p), nil); code != http.StatusForbidden {
			t.Errorf("A reads %s: %d, want 403", p, code)
		}
	}
	if code, _ := fx.post(a, fx.nodeURL(nodeapi.StatusPath(idB)), nodeapi.StatusReport{}); code != http.StatusForbidden {
		t.Errorf("A reports for B: %d, want 403", code)
	}
	if code, _ := fx.post(a, fx.nodeURL(nodeapi.RegisterPath(idB)), nodeapi.Register{Name: "x"}); code != http.StatusForbidden {
		t.Errorf("A registers B: %d, want 403", code)
	}
	if code, _ := fx.send(a, http.MethodGet, fx.nodeURL(nodeapi.AssignmentsPath(idA)), nil); code != http.StatusOK {
		t.Errorf("A reads its own assignments: %d, want 200", code)
	}
}

func TestNodeRoutesRejectBadInput(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	c := fx.nodeClient(cert)
	fx.registerNode(c, id)
	statusURL := fx.nodeURL(nodeapi.StatusPath(id))

	cases := []struct {
		name, method, url, body string
		want                    int
	}{
		{"malformed node ID", http.MethodGet, fx.nodeURL("/node/v1/nodes/Bad_ID/assignments"), "", http.StatusBadRequest},
		{"unknown field", http.MethodPost, statusURL, `{"containers":[],"x":1}`, http.StatusBadRequest},
		{"trailing data", http.MethodPost, statusURL, `{"containers":[]}{}`, http.StatusBadRequest},
		{"empty body", http.MethodPost, statusURL, ``, http.StatusBadRequest},
		{"bad health", http.MethodPost, statusURL, `{"containers":[{"name":"x","health":"fine"}]}`, http.StatusBadRequest},
		{"duplicate names", http.MethodPost, statusURL, `{"containers":[{"name":"x"},{"name":"x"}]}`, http.StatusBadRequest},
		{"oversize body", http.MethodPost, statusURL, `{"containers":[{"name":"` + strings.Repeat("a", nodeapi.MaxStatusBytes) + `"}]}`, http.StatusRequestEntityTooLarge},
		{"register negative cpu", http.MethodPost, fx.nodeURL(nodeapi.RegisterPath(id)), `{"name":"n","cpu":-1}`, http.StatusBadRequest},
		{"register unknown field", http.MethodPost, fx.nodeURL(nodeapi.RegisterPath(id)), `{"name":"n","gpu":1}`, http.StatusBadRequest},
		{"wrong method", http.MethodDelete, statusURL, ``, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		var body []byte
		if tc.body != "" {
			body = []byte(tc.body)
		}
		if code, _ := fx.send(c, tc.method, tc.url, body); code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, code, tc.want)
		}
	}
}

// A node that never registered can't create its own row by reporting.
func TestStatusBeforeRegisterIsNotFound(t *testing.T) {
	fx := newFixture(t, nil)
	id, cert := fx.enroll(fx.issue(time.Hour))
	code, _ := fx.post(fx.nodeClient(cert), fx.nodeURL(nodeapi.StatusPath(id)), nodeapi.StatusReport{})
	if code != http.StatusNotFound {
		t.Fatalf("status before register: %d, want 404", code)
	}
}

// --- plaintext --------------------------------------------------------------

// A plain HTTP request to either port never reaches a handler. The TLS
// listener answers with its own 400 for the mismatch, or closes the
// connection, and it sends nothing from the application.
func TestPlaintextToTheClusterPortsFails(t *testing.T) {
	fx := newFixture(t, nil)
	for _, addr := range []string{fx.nodeAddr, fx.joinAddr} {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", nodeapi.AssignmentsPath("node-x"))
		resp, _ := io.ReadAll(io.LimitReader(conn, 4096))
		conn.Close()
		if bytes.Contains(resp, []byte(" 200 ")) || bytes.Contains(resp, []byte("application/json")) ||
			bytes.Contains(resp, []byte(`"error"`)) {
			t.Errorf("%s answered plaintext with an application reply: %q", addr, resp)
		}
	}
}

// --- rate limit -------------------------------------------------------------

func TestJoinRateLimitRefusesAfterTheLimit(t *testing.T) {
	fx := newFixture(t, NewFailLimiter(2, time.Hour))
	tok := fx.issue(time.Hour)
	forged, err := pki.ParseJoinToken(fx.issue(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	forged.Fingerprint[1] ^= 0x01
	for i := range 2 {
		if _, _, code, _ := fx.tryJoinPinned(fx.ca.Fingerprint(), forged.String()); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i, code)
		}
	}
	if _, _, code, _ := fx.tryJoin(tok); code != http.StatusTooManyRequests {
		t.Fatalf("after the limit: status %d, want 429", code)
	}
	// The refused join did not use up the token.
	if _, err := fx.tokens.Redeem(tok); err != nil {
		t.Fatalf("token was used by a refused join: %v", err)
	}
}

// --- listeners --------------------------------------------------------------

func TestListenBindsOnlyTheAdvertiseAddressAndRefusesWildcards(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()), LocalNodeID: "node-server"}

	for _, adv := range []string{"0.0.0.0", "::", ""} {
		cfg := base
		cfg.Advertise = adv
		if c, err := Listen(cfg); err == nil {
			c.Shutdown()
			t.Errorf("Listen with advertise %q succeeded", adv)
		}
	}

	cfg := base
	cfg.Advertise = "127.0.0.1"
	c, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Shutdown()
	for _, a := range []string{c.NodeAddr(), c.JoinAddr()} {
		host, _, err := net.SplitHostPort(a)
		if err != nil || host != "127.0.0.1" {
			t.Errorf("listener bound %s, want the advertise address 127.0.0.1", a)
		}
	}
}

func TestListenRefusesPortsWithNoRoomForTheJoinPort(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()), LocalNodeID: "node-server",
		Advertise: "127.0.0.1", Port: 65535}
	if _, err := Listen(cfg); err == nil {
		t.Error("port 65535 was accepted, leaving no room for the join port")
	}
}

// caPool returns a pool that holds only ca.
func caPool(ca *pki.CA) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}
