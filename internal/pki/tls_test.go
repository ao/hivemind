package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testMux serves /id (the verified peer's node ID), /check (compares the peer
// with the X-Node-ID header) and / (a plain "ok").
func testMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/id", func(w http.ResponseWriter, r *http.Request) {
		id, err := RequestNodeID(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		fmt.Fprint(w, id)
	})
	mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
		if err := CheckRequestNodeID(r, r.Header.Get("X-Node-ID")); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	})
	return mux
}

// serve starts an HTTPS server on 127.0.0.1:0 with cfg and returns its address.
// The listener is closed when the test ends.
func serve(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: testMux(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func newClient(cfg *tls.Config) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true},
	}
}

// fetch GETs rawURL. If nodeHeader is not empty it is sent as X-Node-ID.
func fetch(c *http.Client, rawURL, nodeHeader string) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, "", err
	}
	if nodeHeader != "" {
		req.Header.Set("X-Node-ID", nodeHeader)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body)), err
}

// clusterFixture is a CA, a server certificate for 127.0.0.1, and a server
// config that requires client certificates from ca.
type clusterFixture struct {
	ca     *CA
	server tls.Certificate
	addr   string
}

func newCluster(t *testing.T) clusterFixture {
	t.Helper()
	ca := newCA(t)
	server := nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)
	return clusterFixture{ca: ca, server: server, addr: serve(t, ServerTLSConfig(ca, server))}
}

func (f clusterFixture) clientCfg(t *testing.T, ca *CA, cert tls.Certificate) *tls.Config {
	t.Helper()
	cfg, err := ClientTLSConfig(ca, cert, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestMutualTLSHandshakeSucceeds(t *testing.T) {
	f := newCluster(t)
	cfg := f.clientCfg(t, f.ca, nodeCert(t, f.ca, "node-a", "", time.Time{}, 0))
	status, body, err := fetch(newClient(cfg), "https://"+f.addr+"/id", "")
	if err != nil {
		t.Fatalf("handshake with valid node cert failed: %v", err)
	}
	if status != http.StatusOK || body != "node-a" {
		t.Fatalf("got %d %q, want 200 node-a", status, body)
	}
}

func TestHandlerChecksRequestedNodeID(t *testing.T) {
	f := newCluster(t)
	c := newClient(f.clientCfg(t, f.ca, nodeCert(t, f.ca, "node-a", "", time.Time{}, 0)))
	base := "https://" + f.addr + "/check"

	if status, body, err := fetch(c, base, "node-a"); err != nil || status != 200 || body != "ok" {
		t.Fatalf("matching ID: %d %q %v", status, body, err)
	}
	if status, body, _ := fetch(c, base, "node-b"); status != http.StatusForbidden || !strings.Contains(body, "does not match") {
		t.Fatalf("other node's ID: got %d %q, want 403 mismatch", status, body)
	}
	if status, _, _ := fetch(c, base, ""); status != http.StatusForbidden {
		t.Fatalf("empty ID: got %d, want 403", status)
	}
}

func TestRejectsClientWithoutCertificate(t *testing.T) {
	f := newCluster(t)
	cfg := f.clientCfg(t, f.ca, nodeCert(t, f.ca, "node-a", "", time.Time{}, 0))
	cfg.Certificates = nil
	if _, _, err := fetch(newClient(cfg), "https://"+f.addr+"/id", ""); err == nil {
		t.Fatal("TLS without a client certificate was accepted")
	}
}

func TestRejectsCertFromOtherCA(t *testing.T) {
	f := newCluster(t)
	other := newCA(t)
	cfg := f.clientCfg(t, f.ca, nodeCert(t, other, "node-x", "", time.Time{}, 0))
	if _, _, err := fetch(newClient(cfg), "https://"+f.addr+"/id", ""); err == nil {
		t.Fatal("certificate from another CA was accepted")
	}
}

func TestRejectsExpiredCert(t *testing.T) {
	f := newCluster(t)
	expired := nodeCert(t, f.ca, "node-a", "", time.Now().Add(-72*time.Hour), time.Hour)
	if _, _, err := fetch(newClient(f.clientCfg(t, f.ca, expired)), "https://"+f.addr+"/id", ""); err == nil {
		t.Fatal("expired certificate was accepted")
	}
}

func TestRejectsCertWithoutNodeID(t *testing.T) {
	f := newCluster(t)
	// A certificate from the right CA, with the right usages, and no node ID.
	noID := rawCert(t, f.ca, &x509.Certificate{
		Subject:     pkix.Name{CommonName: "no-id"},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if _, _, err := fetch(newClient(f.clientCfg(t, f.ca, noID)), "https://"+f.addr+"/id", ""); err == nil {
		t.Fatal("certificate with no node-ID SAN was accepted")
	}
}

func TestRejectsPlaintextHTTP(t *testing.T) {
	f := newCluster(t)
	conn, err := net.DialTimeout("tcp", f.addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /id HTTP/1.1\r\nHost: %s\r\n\r\n", f.addr); err != nil {
		t.Fatal(err)
	}
	// The TLS listener answers a plaintext request with a 400 that Go writes
	// itself. The request must not reach the handler, so no 2xx and no handler
	// output may appear.
	resp, _ := io.ReadAll(conn)
	if bytes.Contains(resp, []byte(" 200 ")) || bytes.Contains(resp, []byte("\r\n\r\nok")) || bytes.Contains(resp, []byte("node-")) {
		t.Fatalf("plaintext request reached the handler: %q", resp)
	}
	if len(resp) > 0 && !bytes.HasPrefix(resp, []byte("HTTP/1.0 400")) && !bytes.HasPrefix(resp, []byte{0x15}) {
		t.Fatalf("unexpected response to plaintext request: %q", resp)
	}
}

func TestClientRejectsServerFromOtherCA(t *testing.T) {
	f := newCluster(t)
	other := newCA(t)
	rogue := serve(t, ServerTLSConfig(other, nodeCert(t, other, "rogue", "127.0.0.1", time.Time{}, 0)))
	cfg := f.clientCfg(t, f.ca, nodeCert(t, f.ca, "node-a", "", time.Time{}, 0))
	if _, _, err := fetch(newClient(cfg), "https://"+rogue+"/id", ""); err == nil {
		t.Fatal("client trusted a server certificate from a different CA")
	}
}

func TestRejectsTLS12(t *testing.T) {
	f := newCluster(t)
	cfg := f.clientCfg(t, f.ca, nodeCert(t, f.ca, "node-a", "", time.Time{}, 0))
	cfg.MaxVersion = tls.VersionTLS12
	if _, _, err := fetch(newClient(cfg), "https://"+f.addr+"/id", ""); err == nil {
		t.Fatal("TLS 1.2 handshake was accepted")
	}
}

func TestJoinHandshakePinsCA(t *testing.T) {
	ca := newCA(t)
	addr := serve(t, JoinServerTLSConfig(nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)))
	cfg, err := JoinClientTLSConfig(ca.Fingerprint(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	status, body, err := fetch(newClient(cfg), "https://"+addr+"/", "")
	if err != nil || status != 200 || body != "ok" {
		t.Fatalf("pinned join handshake: %d %q %v", status, body, err)
	}
}

func TestJoinFingerprintMismatch(t *testing.T) {
	ca := newCA(t)
	other := newCA(t)
	addr := serve(t, JoinServerTLSConfig(nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)))
	cfg, err := JoinClientTLSConfig(other.Fingerprint(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fetch(newClient(cfg), "https://"+addr+"/", "")
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch", err)
	}
}

func TestJoinServerMustSendPinnedCA(t *testing.T) {
	ca := newCA(t)
	leafOnly := nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)
	leafOnly.Certificate = leafOnly.Certificate[:1] // no CA in the chain
	addr := serve(t, JoinServerTLSConfig(leafOnly))
	cfg, err := JoinClientTLSConfig(ca.Fingerprint(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fetch(newClient(cfg), "https://"+addr+"/", "")
	if !errors.Is(err, ErrPinnedCAMissing) {
		t.Fatalf("err = %v, want ErrPinnedCAMissing", err)
	}
}

func TestJoinClientRequiresServerName(t *testing.T) {
	if _, err := JoinClientTLSConfig([32]byte{}, ""); err == nil {
		t.Fatal("empty server name accepted")
	}
	if _, err := ClientTLSConfig(newCA(t), tls.Certificate{}, ""); err == nil {
		t.Fatal("empty server name accepted")
	}
}
