package nodeapi

import (
	"crypto/tls"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ao/hivemind/internal/pki"
)

// issueTLS returns a certificate for id from ca, with advertise as its host
// name when it is not empty.
func issueTLS(t *testing.T, ca *pki.CA, id, advertise string) tls.Certificate {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id, Advertise: advertise})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, ca)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// A request in flight when the worker renews keeps its connection, and that
// connection keeps the certificate it was opened with. After
// ResetConnections, no later request may go out on it. Closing only the idle
// connections, as the worker did before, sends the next requests on the busy
// connection once it is free, with the old certificate.
func TestResetConnectionsKeepsLaterRequestsOffBusyConnections(t *testing.T) {
	ca, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	oldCert := issueTLS(t, ca, "node-aaaaaaaaaaaa", "")
	newCert := issueTLS(t, ca, "node-aaaaaaaaaaaa", "")

	var (
		mu   sync.Mutex
		seen []*big.Int
	)
	started, release := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/seen", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.TLS.PeerCertificates[0].SerialNumber)
		mu.Unlock()
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.TLS = pki.ServerTLSConfig(ca, issueTLS(t, ca, "node-server00000", "127.0.0.1"))
	srv.StartTLS()
	defer srv.Close()

	holder := pki.NewCertHolder(oldCert)
	tlsCfg, err := pki.ClientTLSConfigFrom(ca, holder, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(srv.URL, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseIdleConnections()
	get := func(hc *http.Client, path string) {
		t.Helper()
		resp, err := hc.Get(srv.URL + path)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// One connection, opened with the old certificate, then held by a long
	// request, as the log poll holds one.
	get(c.http, "/seen")
	done := make(chan struct{})
	go func() {
		defer close(done)
		get(c.stream, "/slow")
	}()
	<-started

	holder.Store(newCert)
	c.ResetConnections()
	// The agent keeps calling while the poll is open. Its request opens a
	// new connection, since the only one is busy. In net/http that also ends
	// the effect of CloseIdleConnections, which would otherwise close the
	// busy connection once it went idle.
	get(c.http, "/seen")
	close(release)
	<-done

	// The poll's connection is free now, and the most recently used one.
	for range 3 {
		get(c.http, "/seen")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 5 {
		t.Fatalf("the server saw %d requests on /seen, want 5", len(seen))
	}
	if seen[0].Cmp(oldCert.Leaf.SerialNumber) != 0 {
		t.Fatalf("the first request presented serial %s, want the old certificate's", seen[0])
	}
	for i, s := range seen[1:] {
		if s.Cmp(newCert.Leaf.SerialNumber) != 0 {
			t.Errorf("request %d after the reset presented serial %s, the old certificate's; want the new one", i+1, s)
		}
	}
}
