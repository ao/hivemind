package nodeapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClientFollowsTheServers: with several servers, the client moves past
// one that answers that no server leads, and past one that does not
// answer, keeps to the one that worked, and takes the list a reply names.
func TestClientFollowsTheServers(t *testing.T) {
	var aLeads atomic.Bool
	var hitsA, hitsB atomic.Int32
	a := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA.Add(1)
		if !aLeads.Load() {
			w.Header().Set(NoLeaderHeader, "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"no leader"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer a.Close()
	var bAddr string
	b := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB.Add(1)
		w.Header().Set(ServersHeader, strings.TrimPrefix(a.URL, "https://")+","+bAddr)
		w.WriteHeader(http.StatusNoContent)
	}))
	bAddr = strings.TrimPrefix(b.URL, "https://")
	aAddr := strings.TrimPrefix(a.URL, "https://")
	pool := x509.NewCertPool()
	pool.AddCert(a.Certificate())
	cfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	c, err := NewClient(a.URL, cfg, WithServers([]string{aAddr, bAddr}))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var learned [][]string
	c.OnServers(func(list []string) {
		mu.Lock()
		learned = append(learned, list)
		mu.Unlock()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.RenewLease(ctx, "node-1"); err != nil {
		t.Fatalf("with no leader at the first server: %v", err)
	}
	if c.Server() != bAddr || hitsA.Load() != 1 || hitsB.Load() != 1 {
		t.Fatalf("server %s, hits %d %d", c.Server(), hitsA.Load(), hitsB.Load())
	}
	// It keeps to the server that worked.
	if err := c.RenewLease(ctx, "node-1"); err != nil || hitsA.Load() != 1 || hitsB.Load() != 2 {
		t.Fatalf("second request: %v, hits %d %d", err, hitsA.Load(), hitsB.Load())
	}
	// A reply that names the same list as the client has is not news; one
	// in another order is.
	mu.Lock()
	n := len(learned)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("learned %v, though the list is the one the client had", learned)
	}
	// The server it uses goes away, and the first leads now.
	b.Close()
	aLeads.Store(true)
	if err := c.RenewLease(ctx, "node-1"); err != nil {
		t.Fatalf("with the second server gone: %v", err)
	}
	if c.Server() != aAddr {
		t.Fatalf("server %s, want %s", c.Server(), aAddr)
	}
	// None leads: the request fails as one that reached no server, so the
	// agent backs off as it does while the server is down.
	aLeads.Store(false)
	err = c.RenewLease(ctx, "node-1")
	var u interface{ Unreachable() bool }
	if !errors.As(err, &u) || !u.Unreachable() || !strings.Contains(err.Error(), "no server leads") {
		t.Fatalf("with no leader anywhere: %v", err)
	}
}

// TestOneServerClientIsUnchanged: without a list, a client sends every
// request to its one server, whatever it answers.
func TestOneServerClientIsUnchanged(t *testing.T) {
	var hits atomic.Int32
	a := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set(ServersHeader, "bad address")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer a.Close()
	pool := x509.NewCertPool()
	pool.AddCert(a.Certificate())
	c, err := NewClient(a.URL, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}, WithServers([]string{strings.TrimPrefix(a.URL, "https://")}))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := c.RenewLease(context.Background(), "node-1"); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 || c.Servers() != nil {
		t.Fatalf("hits %d, servers %v", hits.Load(), c.Servers())
	}
}

func TestParseServers(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"10.0.0.5:4484":                   "10.0.0.5:4484",
		"10.0.0.5:4484, hm-2.local:4484":  "10.0.0.5:4484,hm-2.local:4484",
		"10.0.0.5":                        "",
		"a:1,b:2,c:3,d:4,e:5,f:6,g:7,h:8": "",
		"10.0.0.5:4484,bad address:1":     "",
	} {
		if got := strings.Join(ParseServers(in), ","); got != want {
			t.Errorf("ParseServers(%q) = %q, want %q", in, got, want)
		}
	}
}
