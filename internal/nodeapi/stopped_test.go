package nodeapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestClientPassesAServerThatGaveNoConnection: a request whose context
// ends while a server has not given it a connection, as one whose process
// is stopped does not, makes the next request start at the next server,
// which answers at once; before, every request with a context shorter than
// the TLS handshake's limit tried the stopped server first, and failed
// (the ninth soak's first finding).
func TestClientPassesAServerThatGaveNoConnection(t *testing.T) {
	var hits atomic.Int32
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer good.Close()
	// Nothing accepts from this listener: a connect completes in the
	// backlog, and the handshake waits.
	stopped, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stopped.Close()
	pool := x509.NewCertPool()
	pool.AddCert(good.Certificate())
	cfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	c, err := NewClient("https://"+stopped.Addr().String(), cfg,
		WithServers([]string{stopped.Addr().String(), strings.TrimPrefix(good.URL, "https://")}))
	if err != nil {
		t.Fatal(err)
	}
	renew := func() (time.Duration, error) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		start := time.Now()
		err := c.RenewLease(ctx, "node-1")
		return time.Since(start), err
	}
	if _, err := renew(); err == nil {
		t.Fatal("a renewal through a server that never answers the handshake, in 1 s, succeeded")
	}
	if d, err := renew(); err != nil || hits.Load() != 1 || d > 500*time.Millisecond {
		t.Fatalf("the next renewal: %v after %s, %d requests at the server that answers", err, d, hits.Load())
	}
}

// TestHomeClientStartsAtItsOwnServer: while home reports true, a request
// starts at the server of the client's base URL, a server's own listener,
// rather than at the server that answered last, which hangs now.
func TestHomeClientStartsAtItsOwnServer(t *testing.T) {
	var ownLeader, otherHangs atomic.Bool
	own := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ownLeader.Load() {
			w.Header().Set(NoLeaderHeader, "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"no leader"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer own.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if otherHangs.Load() {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer other.Close()
	pool := x509.NewCertPool()
	pool.AddCert(own.Certificate())
	cfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	ownAddr, otherAddr := strings.TrimPrefix(own.URL, "https://"), strings.TrimPrefix(other.URL, "https://")
	c, err := NewClient(own.URL, cfg, WithServers([]string{ownAddr, otherAddr}), WithHome(ownLeader.Load))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.RenewLease(ctx, "node-1"); err != nil || c.Server() != otherAddr {
		t.Fatalf("with no leader at its own server: %v, at %s", err, c.Server())
	}
	ownLeader.Store(true)
	otherHangs.Store(true)
	start := time.Now()
	if err := c.RenewLease(ctx, "node-1"); err != nil || time.Since(start) > time.Second {
		t.Fatalf("with a leader known: %v after %s", err, time.Since(start))
	}
}
