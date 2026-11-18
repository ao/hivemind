package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rotationCAs is an old CA and a new one, as a rotation has them.
func rotationCAs(t *testing.T) (oldCA, newCA *CA) {
	t.Helper()
	dir := t.TempDir()
	oldCA, err := CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	newCA, err = CreateNextCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	return oldCA, newCA
}

// serveTrust serves with a server certificate from serverCA and client
// CAs from the holder.
func serveTrust(t *testing.T, serverCA *CA, clients *TrustHolder) string {
	t.Helper()
	h := NewCertHolder(nodeCert(t, serverCA, "server", "127.0.0.1", time.Time{}, 0))
	return serve(t, ServerTLSConfigTrust(clients, h))
}

func trustClient(t *testing.T, trust *Trust, cert tls.Certificate) *http.Client {
	t.Helper()
	cfg, err := ClientTLSConfigTrust(trust, NewCertHolder(cert), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return newClient(cfg)
}

// A worker checks the server's certificate against every CA it trusts: one
// that trusts only the old CA takes a server certificate from it and not
// from the new one; one that trusts both takes either; one that trusts only
// the new CA takes only the new one's.
func TestWorkerTrustsServerCertificateByItsSet(t *testing.T) {
	oldCA, newCA := rotationCAs(t)
	both := NewTrustHolder(MustTrust(oldCA.Cert, newCA.Cert))
	servers := map[string]string{
		"old": serveTrust(t, oldCA, both),
		"new": serveTrust(t, newCA, both),
	}
	clientCert := nodeCert(t, oldCA, "node-a", "", time.Time{}, 0)
	sets := map[string]*Trust{
		"old":  MustTrust(oldCA.Cert),
		"both": MustTrust(oldCA.Cert, newCA.Cert),
		"new":  MustTrust(newCA.Cert),
	}
	want := map[string]map[string]bool{
		"old":  {"old": true, "new": false},
		"both": {"old": true, "new": true},
		"new":  {"old": false, "new": true},
	}
	for set, trust := range sets {
		for srv, addr := range servers {
			status, body, err := fetch(trustClient(t, trust, clientCert), "https://"+addr+"/id", "")
			ok := err == nil && status == 200 && body == "node-a"
			if ok != want[set][srv] {
				t.Errorf("worker trusting %s, server certificate from the %s CA: ok=%v (%d %q %v), want %v",
					set, srv, ok, status, body, err, want[set][srv])
			}
		}
	}
}

// The server checks client certificates against the set in its holder, read
// at each handshake: a swap takes effect for the next connection, and one
// already open keeps the root it was checked against, which VerifiedRoot
// gives a handler.
func TestServerTrustsClientCertificateBySet(t *testing.T) {
	oldCA, newCA := rotationCAs(t)
	holder := NewTrustHolder(MustTrust(oldCA.Cert))
	addr := serveTrust(t, oldCA, holder)
	both := MustTrust(oldCA.Cert, newCA.Cert)
	fromOld := nodeCert(t, oldCA, "node-a", "", time.Time{}, 0)
	fromNew := nodeCert(t, newCA, "node-b", "", time.Time{}, 0)

	try := func(cert tls.Certificate) bool {
		status, _, err := fetch(trustClient(t, both, cert), "https://"+addr+"/id", "")
		return err == nil && status == 200
	}
	if !try(fromOld) || try(fromNew) {
		t.Fatal("trusting the old CA: want the old CA's certificate only")
	}
	holder.Store(both)
	if !try(fromOld) || !try(fromNew) {
		t.Fatal("trusting both: want both certificates")
	}
	holder.Store(MustTrust(newCA.Cert))
	if try(fromOld) || !try(fromNew) {
		t.Fatal("trusting the new CA: want the new CA's certificate only")
	}
}

func TestVerifiedRootIsTheIssuingCA(t *testing.T) {
	oldCA, newCA := rotationCAs(t)
	got := make(chan *x509.Certificate, 1)
	h := NewCertHolder(nodeCert(t, oldCA, "server", "127.0.0.1", time.Time{}, 0))
	cfg := ServerTLSConfigTrust(NewTrustHolder(MustTrust(oldCA.Cert, newCA.Cert)), h)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- VerifiedRoot(r.TLS)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := trustClient(t, MustTrust(oldCA.Cert), nodeCert(t, newCA, "node-a", "", time.Time{}, 0))
	_, _, _ = fetch(c, "https://"+ln.Addr().String()+"/", "")
	root := <-got
	if root == nil || Fingerprint(root) != newCA.Fingerprint() {
		t.Fatalf("verified root %v, want the new CA", root)
	}
	if VerifiedRoot(nil) != nil || VerifiedRoot(&tls.ConnectionState{}) != nil {
		t.Fatal("VerifiedRoot of no verified chain is not nil")
	}
}

func TestCrossCertificate(t *testing.T) {
	oldCA, newCA := rotationCAs(t)
	cross, err := oldCA.CrossSign(newCA.Cert)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	signer, err := VerifyCross(newCA.Cert, cross, MustTrust(oldCA.Cert), now)
	if err != nil || Fingerprint(signer) != oldCA.Fingerprint() {
		t.Fatalf("VerifyCross with the old CA trusted: %v, %v", signer, err)
	}
	// Signed by a CA the node does not trust.
	if _, err := VerifyCross(newCA.Cert, cross, MustTrust(newCA.Cert), now); !errors.Is(err, ErrCrossUntrusted) {
		t.Fatalf("trusting only the new CA: %v, want ErrCrossUntrusted", err)
	}
	// A cross-certificate for another CA does not vouch for this one.
	_, third := rotationCAs(t)
	if _, err := VerifyCross(third.Cert, cross, MustTrust(oldCA.Cert), now); !errors.Is(err, ErrCrossMismatch) {
		t.Fatalf("for another CA: %v, want ErrCrossMismatch", err)
	}
	// The other way round, as an abort in the renew phase needs it.
	back, err := newCA.CrossSign(oldCA.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCross(oldCA.Cert, back, MustTrust(newCA.Cert), now); err != nil {
		t.Fatalf("the old CA cross-signed by the new: %v", err)
	}
	// Expired by the clock.
	if _, err := VerifyCross(newCA.Cert, cross, MustTrust(oldCA.Cert), now.Add(20*365*24*time.Hour)); !errors.Is(err, ErrCrossMismatch) {
		t.Fatalf("expired cross-certificate: %v", err)
	}
	if _, err := VerifyCross(newCA.Cert, []byte("junk"), MustTrust(oldCA.Cert), now); !errors.Is(err, ErrCrossMismatch) {
		t.Fatalf("junk: %v", err)
	}
	// It is never usable in a chain: the old CA's path length is 0.
	crossCert, _ := x509.ParseCertificate(cross)
	leaf := nodeCert(t, newCA, "node-a", "", time.Time{}, 0).Leaf
	inter := x509.NewCertPool()
	inter.AddCert(crossCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: MustTrust(oldCA.Cert).Pool(), Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("a certificate from the new CA verified against the old CA through the cross-certificate")
	}
	// A node certificate cannot be cross-signed.
	if _, err := oldCA.CrossSign(leaf); !errors.Is(err, ErrNotCA) {
		t.Fatalf("cross-signing a node certificate: %v", err)
	}
}

func TestTrustSet(t *testing.T) {
	oldCA, newCA := rotationCAs(t)
	tr := MustTrust(oldCA.Cert, newCA.Cert, oldCA.Cert)
	if got := tr.Fingerprints(); len(got) != 2 || got[0] != oldCA.Fingerprint() || got[1] != newCA.Fingerprint() {
		t.Fatalf("fingerprints %x", got)
	}
	if !tr.Same(MustTrust(newCA.Cert, oldCA.Cert)) || tr.Same(MustTrust(oldCA.Cert)) {
		t.Fatal("Same")
	}
	leaf := nodeCert(t, newCA, "node-a", "", time.Time{}, 0).Leaf
	if iss := tr.Issuer(leaf); iss == nil || Fingerprint(iss) != newCA.Fingerprint() {
		t.Fatal("Issuer of a certificate from the new CA")
	}
	if MustTrust(oldCA.Cert).Issuer(leaf) != nil {
		t.Fatal("the old CA alone issued a certificate from the new CA")
	}
	back, err := ParseTrustPEM(tr.PEM())
	if err != nil || !back.Same(tr) || back.Fingerprints()[0] != oldCA.Fingerprint() {
		t.Fatalf("PEM round trip: %v", err)
	}
	_, third := rotationCAs(t)
	if _, err := NewTrust(oldCA.Cert, newCA.Cert, third.Cert); err == nil {
		t.Fatal("three CAs accepted")
	}
	if _, err := NewTrust(leaf); !errors.Is(err, ErrNotCA) {
		t.Fatalf("a node certificate as a CA: %v", err)
	}
	if _, err := NewTrust(); err == nil {
		t.Fatal("an empty set accepted")
	}
	bad := append(tr.PEM(), []byte("trailing")...)
	if _, err := ParseTrustPEM(bad); err == nil {
		t.Fatal("trailing data accepted")
	}
	if _, err := ParseTrustPEM(nil); err == nil {
		t.Fatal("no certificate accepted")
	}
	fp := oldCA.Fingerprint()
	if got, err := ParseFingerprintHex(FingerprintHex(fp)); err != nil || got != fp {
		t.Fatalf("fingerprint hex round trip: %v", err)
	}
	for _, s := range []string{"", "ab", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, err := ParseFingerprintHex(s); err == nil {
			t.Fatalf("ParseFingerprintHex(%q) accepted", s)
		}
	}
}

func TestNextCAFiles(t *testing.T) {
	dir := t.TempDir()
	cur, err := CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	next, err := CreateNextCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if next.Fingerprint() == cur.Fingerprint() {
		t.Fatal("the next CA has the current one's key")
	}
	fp := next.Fingerprint()
	if want := "hivemind cluster CA " + FingerprintHex(fp)[:8]; next.Cert.Subject.CommonName != want {
		t.Fatalf("next CA named %q, want %q", next.Cert.Subject.CommonName, want)
	}
	if bytes.Equal(next.Cert.RawSubject, cur.Cert.RawSubject) {
		t.Fatal("the two CAs have the same name")
	}
	loaded, err := LoadNextCA(dir)
	if err != nil || loaded.Fingerprint() != fp {
		t.Fatalf("LoadNextCA: %v", err)
	}
	if again, err := LoadCA(dir); err != nil || again.Fingerprint() != cur.Fingerprint() {
		t.Fatalf("LoadCA after CreateNextCA: %v", err)
	}
	for name, mode := range map[string]os.FileMode{NextCAKeyFile: 0o600, NextCACertFile: 0o644} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v, want mode %04o", name, fi, err, mode)
		}
	}
	if _, err := CreateNextCA(dir); !errors.Is(err, ErrCAExists) {
		t.Fatalf("second CreateNextCA: %v, want ErrCAExists", err)
	}
	if err := os.Chmod(filepath.Join(dir, NextCAKeyFile), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNextCA(dir); !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("LoadNextCA with a readable key: %v", err)
	}
}

// A join token issued before the server's certificate moved names the old
// CA, and its pin says what to do.
func TestJoinPinAfterRotationSaysGetANewToken(t *testing.T) {
	oldCA, newCA := rotationCAs(t)
	addr := serve(t, JoinServerTLSConfig(nodeCert(t, newCA, "server", "127.0.0.1", time.Time{}, 0)))
	cfg, err := JoinClientTLSConfig(oldCA.Fingerprint(), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = fetch(newClient(cfg), "https://"+addr+"/", "")
	if !errors.Is(err, ErrFingerprintMismatch) || !strings.Contains(err.Error(), "hivemind join-token") {
		t.Fatalf("err = %v, want a fingerprint mismatch that names join-token", err)
	}
}
