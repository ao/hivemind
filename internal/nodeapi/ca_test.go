package nodeapi

import (
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/pki"
)

func caPEM(ca *pki.CA) string { return string(ca.CertPEM) }

func testCAs(t *testing.T) (*pki.CA, *pki.CA) {
	t.Helper()
	dir := t.TempDir()
	a, err := pki.CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := pki.CreateNextCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestCABundleParse(t *testing.T) {
	a, b := testCAs(t)
	cross, err := a.CrossSign(b.Cert)
	if err != nil {
		t.Fatal(err)
	}
	crossPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cross}))
	good := CABundle{Phase: CAPhaseRenew, Renew: pki.FingerprintHex(b.Fingerprint()),
		CAs: []BundleCA{{CertPEM: caPEM(a)}, {CertPEM: caPEM(b), CrossPEM: crossPEM}}}
	got, err := good.Parse()
	if err != nil || len(got) != 2 || got[0].Cross != nil || got[1].Cross == nil {
		t.Fatalf("Parse: %v", err)
	}
	leaf := func() string {
		key, _ := pki.NewNodeKey()
		csr, _ := pki.NewCSR(key)
		der, err := a.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-a"})
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}()
	bad := map[string]CABundle{
		"no CA":         {CAs: nil},
		"three CAs":     {CAs: []BundleCA{{CertPEM: caPEM(a)}, {CertPEM: caPEM(b)}, {CertPEM: caPEM(a)}}},
		"twice":         {CAs: []BundleCA{{CertPEM: caPEM(a)}, {CertPEM: caPEM(a)}}},
		"phase":         {Phase: "done", CAs: []BundleCA{{CertPEM: caPEM(a)}}},
		"renew missing": {Renew: pki.FingerprintHex(b.Fingerprint()), CAs: []BundleCA{{CertPEM: caPEM(a)}}},
		"not PEM":       {CAs: []BundleCA{{CertPEM: "junk"}}},
		"two PEMs":      {CAs: []BundleCA{{CertPEM: caPEM(a) + caPEM(b)}}},
		"node cert":     {CAs: []BundleCA{{CertPEM: leaf}}},
		"cross not PEM": {CAs: []BundleCA{{CertPEM: caPEM(a), CrossPEM: "junk"}}},
	}
	for name, bb := range bad {
		if _, err := bb.Parse(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Parse = %v, want ErrInvalid", name, err)
		}
	}
	// The digest follows the phase, the CA to renew under and the CAs, and
	// not the cross-certificate's bytes, which are signed anew at each start.
	again, _ := a.CrossSign(b.Cert)
	same := good
	same.CAs = []BundleCA{{CertPEM: caPEM(a)}, {CertPEM: caPEM(b), CrossPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: again}))}}
	if good.Digest() != same.Digest() || !ValidCADigest(good.Digest()) {
		t.Fatal("the digest changed with the cross-certificate's bytes")
	}
	trust := good
	trust.Phase, trust.Renew = CAPhaseTrust, ""
	if trust.Digest() == good.Digest() {
		t.Fatal("the digest does not follow the phase")
	}
	for _, v := range []string{"", "0123", strings.Repeat("A", 16), strings.Repeat("g", 16)} {
		if ValidCADigest(v) {
			t.Errorf("ValidCADigest(%q)", v)
		}
	}
}

func TestCATrustHeader(t *testing.T) {
	a, b := testCAs(t)
	v := FormatCATrust([][32]byte{a.Fingerprint(), b.Fingerprint()})
	fps, sent := ParseCATrust(v)
	if !sent || len(fps) != 2 || fps[0] != a.Fingerprint() || fps[1] != b.Fingerprint() {
		t.Fatalf("round trip: %x %v", fps, sent)
	}
	if fps, sent := ParseCATrust(""); sent || fps != nil {
		t.Fatal("an absent header is sent")
	}
	for _, v := range []string{"junk", v + "," + v + "," + v} {
		if fps, sent := ParseCATrust(v); !sent || fps != nil {
			t.Errorf("ParseCATrust(%q) = %x, %v; want no fingerprints, sent", v, fps, sent)
		}
	}
}

func TestCARefusalsAreTheirErrors(t *testing.T) {
	for msg, want := range map[string]error{CAUntrustedMessage: ErrCAUntrusted, CASupersededMessage: ErrCASuperseded} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
		}))
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if got := statusError(resp); !errors.Is(got, want) {
			t.Errorf("403 %q: %v, want %v", msg, got, want)
		}
		resp.Body.Close()
		srv.Close()
	}
}
