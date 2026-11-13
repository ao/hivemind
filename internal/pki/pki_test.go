package pki

import (
	"bytes"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newCA makes a CA in a fresh temp dir.
func newCA(t *testing.T) *CA {
	t.Helper()
	ca, err := CreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("CreateCA: %v", err)
	}
	return ca
}

// nodeCert generates a key and a CSR, then issues a certificate for id. Empty
// advertise leaves the SAN out. now zero means the current time.
func nodeCert(t *testing.T, ca *CA, id, advertise string, now time.Time, lifetime time.Duration) tls.Certificate {
	t.Helper()
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, NodeCertOptions{NodeID: id, Advertise: advertise, Now: now, Lifetime: lifetime})
	if err != nil {
		t.Fatalf("IssueNodeCert(%q): %v", id, err)
	}
	cert, err := NewTLSCertificate(der, key, ca)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// rawCert signs a certificate with the CA whose template the test controls.
// It is for building certificates IssueNodeCert would refuse to make.
func rawCert(t *testing.T, ca *CA, tmpl *x509.Certificate) tls.Certificate {
	t.Helper()
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	serial, err := randomSerial()
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = serial
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key}
}

func TestCreateCAWritesKeyAt0600(t *testing.T) {
	dir := t.TempDir()
	ca, err := CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("ca.key mode = %04o, want 0600", got)
	}
	if _, err := os.Stat(filepath.Join(dir, CACertFile)); err != nil {
		t.Fatalf("ca.crt missing: %v", err)
	}
	loaded, err := LoadCA(dir)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	if loaded.Fingerprint() != ca.Fingerprint() {
		t.Fatal("loaded CA has a different fingerprint")
	}
}

func TestCreateCARefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateCA(dir); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCA(dir); !errors.Is(err, ErrCAExists) {
		t.Fatalf("second CreateCA err = %v, want ErrCAExists", err)
	}
	after, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("second CreateCA changed ca.key")
	}
}

func TestCreateCAWithOnlyCertPresentLeavesKeyAlone(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, CACertFile), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCA(dir); !errors.Is(err, ErrCAExists) {
		t.Fatalf("err = %v, want ErrCAExists", err)
	}
	if _, err := os.Stat(filepath.Join(dir, CAKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ca.key was created next to an existing ca.crt (stat err %v)", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil || string(got) != "existing" {
		t.Fatalf("existing ca.crt was modified: %q, %v", got, err)
	}
}

func TestLoadCARejectsLooseKeyMode(t *testing.T) {
	dir := t.TempDir()
	if _, err := CreateCA(dir); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, CAKeyFile)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		if err := os.Chmod(keyPath, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCA(dir); !errors.Is(err, ErrKeyPermissions) {
			t.Fatalf("mode %04o: err = %v, want ErrKeyPermissions", mode, err)
		}
	}
	if err := os.Chmod(keyPath, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(dir); err != nil {
		t.Fatalf("mode 0400 should load: %v", err)
	}
}

func TestLoadCARejectsKeyMismatch(t *testing.T) {
	a := t.TempDir()
	b := t.TempDir()
	if _, err := CreateCA(a); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateCA(b); err != nil {
		t.Fatal(err)
	}
	// Pair the key from a with the certificate from b.
	keyA, err := os.ReadFile(filepath.Join(a, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, CAKeyFile), keyA, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(b); err == nil {
		t.Fatal("LoadCA accepted a key that does not match the certificate")
	}
}

func TestCAParameters(t *testing.T) {
	ca := newCA(t)
	if ca.Key.Curve != elliptic.P256() {
		t.Fatal("CA key is not P-256")
	}
	if !ca.Cert.IsCA || !ca.Cert.BasicConstraintsValid {
		t.Fatal("CA certificate is not marked as a CA")
	}
	if ca.Cert.Subject.CommonName != ca.Cert.Issuer.CommonName {
		t.Fatal("CA is not self-signed")
	}
	life := ca.Cert.NotAfter.Sub(ca.Cert.NotBefore)
	if life < 9*365*24*time.Hour || life > 11*365*24*time.Hour {
		t.Fatalf("CA lifetime = %s, want about 10 years", life)
	}
}

func TestIssueNodeCertCarriesIDAndUsages(t *testing.T) {
	ca := newCA(t)
	cert := nodeCert(t, ca, "node-a", "10.0.0.5", time.Time{}, 0)
	leaf := cert.Leaf

	id, err := NodeIDFromCert(leaf)
	if err != nil || id != "node-a" {
		t.Fatalf("NodeIDFromCert = %q, %v; want node-a", id, err)
	}
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != "hivemind://node/node-a" {
		t.Fatalf("URIs = %v", leaf.URIs)
	}
	if !hasExtKeyUsage(leaf, x509.ExtKeyUsageClientAuth) || !hasExtKeyUsage(leaf, x509.ExtKeyUsageServerAuth) {
		t.Fatalf("ExtKeyUsage = %v, want client and server auth", leaf.ExtKeyUsage)
	}
	if leaf.IsCA {
		t.Fatal("node certificate is a CA")
	}
	if got := leaf.NotAfter.Sub(leaf.NotBefore); got > NodeCertLifetime+clockSkew+time.Minute {
		t.Fatalf("node certificate lifetime = %s, want at most 1 year", got)
	}
	if len(leaf.IPAddresses) != 1 || leaf.IPAddresses[0].String() != "10.0.0.5" {
		t.Fatalf("IP SANs = %v, want 10.0.0.5", leaf.IPAddresses)
	}
	if err := leaf.VerifyHostname("10.0.0.5"); err != nil {
		t.Fatalf("advertise IP not verifiable: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: caPool(ca)}); err != nil {
		t.Fatalf("leaf does not verify against CA: %v", err)
	}
}

func TestIssueNodeCertDNSAdvertise(t *testing.T) {
	ca := newCA(t)
	cert := nodeCert(t, ca, "srv", "hivemind.example.internal", time.Time{}, 0)
	if len(cert.Leaf.DNSNames) != 1 || cert.Leaf.DNSNames[0] != "hivemind.example.internal" {
		t.Fatalf("DNS SANs = %v", cert.Leaf.DNSNames)
	}
}

func TestIssueNodeCertRejectsBadInput(t *testing.T) {
	ca := newCA(t)
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	badCSR := append([]byte(nil), csr...)
	badCSR[len(badCSR)-1] ^= 0xff // breaks the CSR self-signature

	cases := []struct {
		name string
		csr  []byte
		opt  NodeCertOptions
		want error
	}{
		{"empty id", csr, NodeCertOptions{NodeID: ""}, ErrInvalidNodeID},
		{"upper case id", csr, NodeCertOptions{NodeID: "Node-A"}, ErrInvalidNodeID},
		{"slash in id", csr, NodeCertOptions{NodeID: "a/b"}, ErrInvalidNodeID},
		{"lifetime over a year", csr, NodeCertOptions{NodeID: "a", Lifetime: NodeCertLifetime + time.Hour}, ErrInvalidLifetime},
		{"negative lifetime", csr, NodeCertOptions{NodeID: "a", Lifetime: -time.Hour}, ErrInvalidLifetime},
		{"bad advertise", csr, NodeCertOptions{NodeID: "a", Advertise: "host:4484"}, ErrInvalidAdvertise},
		{"garbage csr", []byte("not a csr"), NodeCertOptions{NodeID: "a"}, ErrInvalidCSR},
		{"tampered csr", badCSR, NodeCertOptions{NodeID: "a"}, ErrInvalidCSR},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ca.IssueNodeCert(c.csr, c.opt)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestNodeIDFromCertRejectsMissingAndAmbiguous(t *testing.T) {
	ca := newCA(t)
	noID := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "x"},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	c := rawCert(t, ca, noID)
	if _, err := NodeIDFromCert(mustParse(t, c.Certificate[0])); !errors.Is(err, ErrNoNodeID) {
		t.Fatalf("no URI: err = %v, want ErrNoNodeID", err)
	}

	two := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "x"},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{mustURL(t, "hivemind://node/a"), mustURL(t, "hivemind://node/b")},
	}
	c = rawCert(t, ca, two)
	if _, err := NodeIDFromCert(mustParse(t, c.Certificate[0])); !errors.Is(err, ErrMultipleNodeIDs) {
		t.Fatalf("two IDs: err = %v, want ErrMultipleNodeIDs", err)
	}

	bad := &x509.Certificate{
		Subject:     pkix.Name{CommonName: "x"},
		NotBefore:   time.Now().Add(-time.Minute),
		NotAfter:    time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{mustURL(t, "hivemind://node/Bad_ID")},
	}
	c = rawCert(t, ca, bad)
	if _, err := NodeIDFromCert(mustParse(t, c.Certificate[0])); !errors.Is(err, ErrInvalidNodeID) {
		t.Fatalf("malformed ID: err = %v, want ErrInvalidNodeID", err)
	}
}

func TestCheckPeerNodeIDOnConnectionState(t *testing.T) {
	ca := newCA(t)
	cert := nodeCert(t, ca, "node-a", "", time.Time{}, 0)
	cs := tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert.Leaf},
		VerifiedChains:   [][]*x509.Certificate{{cert.Leaf, ca.Cert}},
	}
	if err := CheckPeerNodeID(cs, "node-a"); err != nil {
		t.Fatalf("matching ID: %v", err)
	}
	if err := CheckPeerNodeID(cs, "node-b"); !errors.Is(err, ErrNodeIDMismatch) {
		t.Fatalf("other ID: err = %v, want ErrNodeIDMismatch", err)
	}
	if err := CheckPeerNodeID(cs, "NODE-A"); !errors.Is(err, ErrInvalidNodeID) {
		t.Fatalf("invalid expected ID: err = %v, want ErrInvalidNodeID", err)
	}
	unverified := tls.ConnectionState{PeerCertificates: cs.PeerCertificates}
	if err := CheckPeerNodeID(unverified, "node-a"); !errors.Is(err, ErrNoPeerCert) {
		t.Fatalf("unverified peer: err = %v, want ErrNoPeerCert", err)
	}
}

func hasExtKeyUsage(c *x509.Certificate, want x509.ExtKeyUsage) bool {
	for _, u := range c.ExtKeyUsage {
		if u == want {
			return true
		}
	}
	return false
}

func mustParse(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
