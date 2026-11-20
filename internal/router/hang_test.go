package router

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// hangingRoutes is a Routes whose reads hang until their context ends, as
// a read through a server whose process is stopped does, while hang
// reports true at the read's start; the others return a table of host.
type hangingRoutes struct {
	mu    sync.Mutex
	reads int
	hang  func(read int) bool
	host  string
}

func (h *hangingRoutes) routes(ctx context.Context) (Table, error) {
	h.mu.Lock()
	h.reads++
	n := h.reads
	h.mu.Unlock()
	if h.hang(n) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return Table{h.host: {}}, nil
}

// hangingRouter serves a router whose table comes from h, with a
// certificate for h.host in its storage, and returns it with a TLS config
// that trusts that certificate.
func hangingRouter(t *testing.T, h *hangingRoutes) (*Router, *tls.Config) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts, err := tlsstore.New(tlsstore.StoreBackend{St: st, NodeID: "node-a"}, tlsstore.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := New(Config{Addr: "127.0.0.1", Routes: h.routes, Storage: ts, Email: "ops@example.com",
		CA: "https://acme.invalid/dir", Logger: log, Metrics: NewMetrics(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// A table the poll reads has its certificates loaded from storage,
	// as ManageAsync does, without the ACME CA.
	rt.manage = func(ctx context.Context, hosts []string) error {
		for _, host := range hosts {
			if _, err := rt.magic.CacheManagedCertificate(ctx, host); err != nil {
				return err
			}
		}
		return nil
	}
	iss := newTestIssuer(t)
	rt.magic.Issuers = []certmagic.Issuer{iss}
	iss.store(t, ts, h.host, time.Now().Add(-time.Minute), time.Now().Add(24*time.Hour))
	go func() { _ = rt.Serve() }()
	t.Cleanup(func() { _ = rt.Shutdown() })
	pool := x509.NewCertPool()
	pool.AddCert(iss.ca)
	return rt, &tls.Config{RootCAs: pool, ServerName: h.host}
}

// handshake dials the router's https listener, and returns the error of a
// handshake that did not succeed by deadline.
func handshake(rt *Router, cfg *tls.Config, deadline time.Time) error {
	d := &net.Dialer{Deadline: deadline}
	conn, err := tls.DialWithDialer(d, "tcp", rt.HTTPSAddr(), cfg)
	if err == nil {
		conn.Close()
	}
	return err
}

// TestStartReadsAgainWhenAReadHangs: a starting router whose first read of
// the table hangs, as one through a server whose process is stopped does,
// reads it again after 1 s, and serves the handshake that waited within 2 s
// of its start. In the ninth soak that read had the whole 5 s of the
// start's wait, though the server's own store had the table after 1.7 s.
func TestStartReadsAgainWhenAReadHangs(t *testing.T) {
	h := &hangingRoutes{host: "www.example.com", hang: func(read int) bool { return read == 1 }}
	start := time.Now()
	rt, cfg := hangingRouter(t, h)
	if err := handshake(rt, cfg, start.Add(2*time.Second)); err != nil {
		t.Fatalf("a handshake while the first read hangs: %v after %s", err, time.Since(start))
	}
}

// TestRouterWithoutATableReadsAgainSoon: a router that started without a
// table, its reads hanging for 5.5 s, reads it again within 1.5 s of the
// first read that would not hang, rather than at the end of a 10 s read
// started before (the ninth soak: 10 s of failed handshakes, 6.9 s of them
// after the server's own store had the table).
func TestRouterWithoutATableReadsAgainSoon(t *testing.T) {
	start := time.Now()
	until := start.Add(5500 * time.Millisecond)
	h := &hangingRoutes{host: "www.example.com", hang: func(int) bool { return time.Now().Before(until) }}
	rt, cfg := hangingRouter(t, h)
	for {
		err := handshake(rt, cfg, time.Now().Add(time.Second))
		if err == nil {
			break
		}
		if time.Since(until) > 1500*time.Millisecond {
			t.Fatalf("no handshake %s after the reads stopped hanging: %v", time.Since(until).Round(time.Millisecond), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
