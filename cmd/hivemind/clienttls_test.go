package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
)

// testCA is a CA made for one test with crypto/x509.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a server certificate and key, in PEM, for the IPs and names.
func (ca *testCA) issue(t *testing.T, ips []net.IP, names ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "hivemind api"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  ips,
		DNSNames:     names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})
}

func writeFile(t *testing.T, path string, b []byte) string {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// tlsAPI serves a stand-in for GET /v1/status over TLS on 127.0.0.1, with a
// certificate from ca for 127.0.0.1 and hivemind.test only.
func tlsAPI(t *testing.T, ca *testCA, token string) *httptest.Server {
	t.Helper()
	certPEM, keyPEM := ca.issue(t, []net.IP{net.ParseIP("127.0.0.1")}, "hivemind.test")
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]\n"))
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func TestCACertTrustsAPrivateCA(t *testing.T) {
	home := isolate(t)
	ca := newTestCA(t, "test ca")
	tok, _ := auth.NewToken()
	ts := tlsAPI(t, ca, tok)
	caFile := writeFile(t, filepath.Join(home, "ca.crt"), ca.pem)
	otherFile := writeFile(t, filepath.Join(home, "other.crt"), newTestCA(t, "other ca").pem)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	byIP := ts.URL // https://127.0.0.1:<port>

	// The right CA, from the flag and from the environment.
	if out, _, err := run(t, "", "status", "--server", byIP, "--token", tok, "--ca-cert", caFile); err != nil || !strings.Contains(out, "No apps.") {
		t.Fatalf("status --ca-cert: %q, %v", out, err)
	}
	t.Setenv(envCACert, caFile)
	if out, _, err := run(t, "", "status", "--server", byIP, "--token", tok); err != nil || !strings.Contains(out, "No apps.") {
		t.Fatalf("status with %s: %q, %v", envCACert, out, err)
	}
	os.Unsetenv(envCACert)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"system roots only", []string{"--server", byIP}, "signed by an unknown authority; pass --ca-cert"},
		{"another CA", []string{"--server", byIP, "--ca-cert", otherFile}, "is not signed by a CA in " + otherFile},
		// The certificate names 127.0.0.1 and hivemind.test, not localhost.
		{"wrong host name", []string{"--server", "https://localhost:" + strconv.Itoa(port), "--ca-cert", caFile}, "does not name localhost"},
		{"plain http", []string{"--server", "http://127.0.0.1:" + strconv.Itoa(port), "--ca-cert", caFile}, "applies to an https server"},
		{"missing file", []string{"--server", byIP, "--ca-cert", filepath.Join(home, "nope.crt")}, "no such file"},
		{"no certificate", []string{"--server", byIP, "--ca-cert", writeFile(t, filepath.Join(home, "empty.pem"), []byte("x\n"))}, "holds no PEM certificate"},
	}
	for _, c := range cases {
		_, _, err := run(t, "", append([]string{"status", "--token", tok}, c.args...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// The client config's ca-cert is used when no flag or variable is set, and a
// relative path is read from the config file's directory.
func TestCACertFromClientConfig(t *testing.T) {
	home := isolate(t)
	ca := newTestCA(t, "test ca")
	tok, _ := auth.NewToken()
	ts := tlsAPI(t, ca, tok)
	dir := filepath.Join(home, "cfg")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "api-ca.crt"), ca.pem)
	cc := writeFile(t, filepath.Join(dir, "config.yaml"), []byte("server: "+ts.URL+"\nca-cert: api-ca.crt\n"))
	if out, _, err := run(t, "", "status", "--client-config", cc, "--token", tok); err != nil || !strings.Contains(out, "No apps.") {
		t.Fatalf("status from the client config: %q, %v", out, err)
	}
	// The flag wins over the config.
	other := writeFile(t, filepath.Join(home, "other.crt"), newTestCA(t, "other").pem)
	if _, _, err := run(t, "", "status", "--client-config", cc, "--token", tok, "--ca-cert", other); err == nil ||
		!strings.Contains(err.Error(), "not signed by a CA in") {
		t.Fatalf("--ca-cert over the client config: %v", err)
	}
}

// --ca-cert can name the served certificate itself: a certificate in the
// trust pool is accepted as a root. That is what init records.
func TestCACertCanPinTheServedCertificate(t *testing.T) {
	home := isolate(t)
	ca := newTestCA(t, "test ca")
	certPEM, _ := ca.issue(t, []net.IP{net.ParseIP("127.0.0.1")})
	leafFile := writeFile(t, filepath.Join(home, "leaf.crt"), certPEM)
	tok, _ := auth.NewToken()
	ts := tlsAPI(t, ca, tok)
	// leaf.crt is a different certificate from the one the server presents,
	// so trusting it alone must fail; the server's CA is what works.
	if _, _, err := run(t, "", "status", "--server", ts.URL, "--token", tok, "--ca-cert", leafFile); err == nil {
		t.Fatal("an unrelated leaf was accepted as a root")
	}
	served := ts.Certificate()
	servedFile := writeFile(t, filepath.Join(home, "served.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: served.Raw}))
	if out, _, err := run(t, "", "status", "--server", ts.URL, "--token", tok, "--ca-cert", servedFile); err != nil || !strings.Contains(out, "No apps.") {
		t.Fatalf("status pinned to the served certificate: %q, %v", out, err)
	}
}

func TestAPITLSConfigFloor(t *testing.T) {
	u := mustURL(t, "https://127.0.0.1:4483")
	cfg, err := apiTLSConfig(u, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || cfg.InsecureSkipVerify || cfg.ServerName != "127.0.0.1" || cfg.RootCAs != nil {
		t.Fatalf("TLS config %+v", cfg)
	}
}

// init with --api-tls-cert records the certificate in the client config.
func TestInitRecordsTheAPICertificate(t *testing.T) {
	home := isolate(t)
	ca := newTestCA(t, "test ca")
	certPEM, keyPEM := ca.issue(t, []net.IP{net.ParseIP("127.0.0.1")})
	cert := writeFile(t, filepath.Join(home, "api.crt"), certPEM)
	key := writeFile(t, filepath.Join(home, "api.key"), keyPEM)
	cc := filepath.Join(home, "client.yaml")
	out, _, err := run(t, "", "init", "--data-dir", filepath.Join(home, "d"), "--client-config", cc,
		"--api-tls-cert", cert, "--api-tls-key", key)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cc)
	if !strings.Contains(string(b), "server: https://127.0.0.1:4483") || !strings.Contains(string(b), "ca-cert: "+cert) {
		t.Fatalf("client config:\n%s", b)
	}
	if strings.Contains(out, "does not name") {
		t.Fatalf("init warned about a certificate that names 127.0.0.1:\n%s", out)
	}

	// A certificate without 127.0.0.1 gets a note.
	home2 := filepath.Join(home, "two")
	certPEM, keyPEM = ca.issue(t, nil, "api.example")
	cert = writeFile(t, filepath.Join(home, "api2.crt"), certPEM)
	key = writeFile(t, filepath.Join(home, "api2.key"), keyPEM)
	out, _, err = run(t, "", "init", "--data-dir", filepath.Join(home2, "d"), "--client-config", filepath.Join(home2, "c.yaml"),
		"--api-tls-cert", cert, "--api-tls-key", key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "does not name 127.0.0.1") {
		t.Fatalf("init did not warn about the name:\n%s", out)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
