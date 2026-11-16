package pki

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRenewDueAtTwoThirdsOfLifetime(t *testing.T) {
	ca := newCA(t)
	t0 := time.Now().Truncate(time.Second)
	cert := nodeCert(t, ca, "node-a", "", t0, 300*time.Hour).Leaf
	if got, want := RenewAt(cert), t0.Add(200*time.Hour); !got.Equal(want) {
		t.Fatalf("RenewAt = %s, want %s", got, want)
	}
	for _, c := range []struct {
		at   time.Duration
		want bool
	}{
		{0, false},
		{199*time.Hour + 59*time.Minute, false},
		{200*time.Hour - time.Second, false},
		{200 * time.Hour, true},
		{299 * time.Hour, true},
	} {
		if got := RenewDue(cert, t0.Add(c.at)); got != c.want {
			t.Errorf("RenewDue at +%s = %v, want %v", c.at, got, c.want)
		}
	}
	// A year-long certificate is due at day 243, with about four months left.
	year := nodeCert(t, ca, "node-b", "", t0, 0).Leaf
	if left := year.NotAfter.Sub(RenewAt(year)); left < 120*24*time.Hour {
		t.Fatalf("a 365-day certificate renews with %s left, want at least 120 days", left)
	}
}

func TestRenewCheckIntervalIsBounded(t *testing.T) {
	ca := newCA(t)
	if d := RenewCheckInterval(nodeCert(t, ca, "a", "", time.Time{}, 0).Leaf); d != time.Hour {
		t.Errorf("365-day certificate: interval %s, want 1h", d)
	}
	if d := RenewCheckInterval(nodeCert(t, ca, "a", "", time.Time{}, 5*time.Minute).Leaf); d != 15*time.Second {
		t.Errorf("5-minute certificate: interval %s, want 15s", d)
	}
	if d := RenewCheckInterval(nodeCert(t, ca, "a", "", time.Time{}, 2*time.Second).Leaf); d != time.Second {
		t.Errorf("2-second certificate: interval %s, want 1s", d)
	}
}

func TestCheckValidAt(t *testing.T) {
	ca := newCA(t)
	t0 := time.Now()
	cert := nodeCert(t, ca, "a", "", t0, time.Hour).Leaf
	if err := CheckValidAt(cert, t0.Add(30*time.Minute)); err != nil {
		t.Fatalf("within its lifetime: %v", err)
	}
	if err := CheckValidAt(cert, t0.Add(time.Hour+time.Second)); !errors.Is(err, ErrCertExpired) {
		t.Fatalf("after NotAfter: %v, want ErrCertExpired", err)
	}
	if err := CheckValidAt(cert, t0.Add(-time.Hour)); !errors.Is(err, ErrCertNotYetValid) {
		t.Fatalf("before NotBefore: %v, want ErrCertNotYetValid", err)
	}
}

// In the CA's last year a certificate ends with the CA, so renewals keep
// working; with less than an hour of CA left, issuance is refused.
func TestIssueNodeCertEndsWithCA(t *testing.T) {
	ca := newCA(t)
	late := ca.Cert.NotAfter.Add(-100 * 24 * time.Hour)
	cert := nodeCert(t, ca, "a", "", late, 0).Leaf
	if !cert.NotAfter.Equal(ca.Cert.NotAfter) {
		t.Fatalf("NotAfter %s, want the CA's %s", cert.NotAfter, ca.Cert.NotAfter)
	}
	key, _ := NewNodeKey()
	csr, _ := NewCSR(key)
	_, err := ca.IssueNodeCert(csr, NodeCertOptions{NodeID: "a", Now: ca.Cert.NotAfter.Add(-30 * time.Minute)})
	if !errors.Is(err, ErrInvalidLifetime) {
		t.Fatalf("30 minutes before the CA expires: %v, want ErrInvalidLifetime", err)
	}
}

// serialMux answers with the serial of the client certificate.
func serialMux() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.TLS.PeerCertificates[0].SerialNumber.String())
	})
}

func serveHandler(t *testing.T, cfg *tls.Config, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(tls.NewListener(ln, cfg)) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// A server built on a CertHolder presents the certificate stored last to
// each new handshake, with no restart.
func TestServerCertHolderHotSwap(t *testing.T) {
	ca := newCA(t)
	first := nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)
	h := NewCertHolder(first)
	addr := serve(t, ServerTLSConfigFrom(ca, h))
	client := nodeCert(t, ca, "node-a", "", time.Time{}, 0)
	cfg, err := ClientTLSConfig(ca, client, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	serverSerial := func() string {
		conn, err := tls.Dial("tcp", addr, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	if got := serverSerial(); got != first.Leaf.SerialNumber.String() {
		t.Fatalf("server presented serial %s, want the first certificate", got)
	}
	second := nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)
	h.Store(second)
	if got := serverSerial(); got != second.Leaf.SerialNumber.String() {
		t.Fatalf("after the swap the server presented serial %s, want the second certificate", got)
	}
}

// A client built on a CertHolder presents the certificate stored last on
// each new connection.
func TestClientCertHolderHotSwap(t *testing.T) {
	ca := newCA(t)
	addr := serveHandler(t, ServerTLSConfig(ca, nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)), serialMux())
	first := nodeCert(t, ca, "node-a", "", time.Time{}, 0)
	h := NewCertHolder(first)
	cfg, err := ClientTLSConfigFrom(ca, h, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(cfg) // no keep-alives: each request is a new handshake
	_, body, err := fetch(c, "https://"+addr+"/", "")
	if err != nil || body != first.Leaf.SerialNumber.String() {
		t.Fatalf("first request: %q, %v", body, err)
	}
	second := nodeCert(t, ca, "node-a", "", time.Time{}, 0)
	h.Store(second)
	_, body, err = fetch(c, "https://"+addr+"/", "")
	if err != nil || body != second.Leaf.SerialNumber.String() {
		t.Fatalf("after the swap: %q, %v; want serial %s", body, err, second.Leaf.SerialNumber)
	}
}

// An expired client certificate fails the handshake, holder or not.
func TestCertHolderExpiredClientCertRefused(t *testing.T) {
	ca := newCA(t)
	addr := serveHandler(t, ServerTLSConfig(ca, nodeCert(t, ca, "server", "127.0.0.1", time.Time{}, 0)), serialMux())
	expired := nodeCert(t, ca, "node-a", "", time.Now().Add(-2*time.Hour), time.Hour)
	cfg, err := ClientTLSConfigFrom(ca, NewCertHolder(expired), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := fetch(newClient(cfg), "https://"+addr+"/", ""); err == nil {
		t.Fatal("an expired client certificate was accepted")
	}
}

// A certificate is overdue for renewal a grace after its renewal time: a
// tenth of its lifetime, at most a day. For a year's certificate that is day
// 244 and 8 hours.
func TestRenewOverdue(t *testing.T) {
	day := 24 * time.Hour
	issued := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	year := issued.Add(365 * day)
	short := issued.Add(10 * time.Minute)
	for _, c := range []struct {
		issued, notAfter, now time.Time
		want                  bool
	}{
		{issued, year, issued.Add(243 * day), false},
		{issued, year, issued.Add(244 * day), false},
		{issued, year, issued.Add(244*day + 9*time.Hour), true},
		{issued, year, year.Add(time.Hour), true},
		{issued, short, issued.Add(7*time.Minute + 30*time.Second), false},
		{issued, short, issued.Add(7*time.Minute + 45*time.Second), true},
		{time.Time{}, year, year, false},
		{year, issued, year, false},
	} {
		if got := RenewOverdue(c.issued, c.notAfter, c.now); got != c.want {
			t.Errorf("RenewOverdue(issued, %s, %s after issue) = %v, want %v", c.notAfter.Sub(c.issued), c.now.Sub(c.issued), got, c.want)
		}
	}
}
