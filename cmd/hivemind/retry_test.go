package main

import (
	"context"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
)

// countingServer answers 200 with {} and counts its requests.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

func testClient(t *testing.T, servers ...string) *apiClient {
	t.Helper()
	c, err := (&clientFlags{server: strings.Join(servers, ","), token: "tok"}).client()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestCLISendsAgainOnlyWhatIsSafe: a server whose connection breaks after
// the request was sent may have acted on it, so a write is not sent to the
// next server, and the error says it may have been applied; a GET is. A
// server that cannot be connected to has done nothing, so a write goes to
// the next (the eighth soak's seventh finding).
func TestCLISendsAgainOnlyWhatIsSafe(t *testing.T) {
	isolate(t)
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
			c.Close()
		}
	}))
	defer cut.Close()
	good, hits := countingServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + ln.Addr().String()
	ln.Close()
	ctx := context.Background()

	err = testClient(t, cut.URL, good.URL).call(ctx, http.MethodPost, "/v1/registries", map[string]string{"host": "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "may or may not have been applied") || hits.Load() != 0 {
		t.Fatalf("a write whose connection broke after it was sent: %v; the next server got %d requests, want 0", err, hits.Load())
	}
	if err := testClient(t, cut.URL, good.URL).call(ctx, http.MethodGet, "/v1/status", nil, nil); err != nil || hits.Load() != 1 {
		t.Fatalf("a GET whose connection broke: %v; the next server got %d requests, want 1", err, hits.Load())
	}
	if err := testClient(t, refused, good.URL).call(ctx, http.MethodPost, "/v1/registries", map[string]string{"host": "x"}, nil); err != nil || hits.Load() != 2 {
		t.Fatalf("a write to a server that refused the connection: %v; the next server got %d requests, want 2", err, hits.Load())
	}
}

// TestCLITriesAgainWhileNoServerLeads: while a server answers that no
// server leads, as during an election, the CLI tries again for a few
// seconds, and gets the answer of the leader once there is one; it gives
// up after leaderWait; and a 503 that is not marked, such as a write whose
// outcome is unknown, is not sent again.
func TestCLITriesAgainWhileNoServerLeads(t *testing.T) {
	isolate(t)
	wait := leaderWait
	leaderWait = 1500 * time.Millisecond
	t.Cleanup(func() { leaderWait = wait })
	ctx := context.Background()

	var asked atomic.Int32
	electing := time.Now().Add(time.Second)
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if time.Now().Before(electing) {
			w.Header().Set(nodeapi.NoLeaderHeader, "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"the leader, node-a at 10.0.0.1:4486, did not answer this server: dial tcp: connection refused"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer follower.Close()
	if err := testClient(t, follower.URL).call(ctx, http.MethodPost, "/v1/registries", map[string]string{"host": "x"}, nil); err != nil {
		t.Fatalf("a write sent while no server led, 1 s before one did: %v", err)
	}
	if n := asked.Load(); n < 2 {
		t.Fatalf("the server was asked %d times", n)
	}

	never := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(nodeapi.NoLeaderHeader, "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no leader: this server reaches 1 of the 3 servers, and a write needs 2"}`))
	}))
	defer never.Close()
	start := time.Now()
	err := testClient(t, never.URL).call(ctx, http.MethodGet, "/v1/status", nil, nil)
	if d := time.Since(start); err == nil || !strings.Contains(err.Error(), "no leader") || d < leaderWait || d > leaderWait+2*time.Second {
		t.Fatalf("with no leader at all: %v after %s", err, d)
	}

	var unknown atomic.Int32
	outcome := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		unknown.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"the leader changed while the write was in the log, so it may or may not have been applied"}`))
	}))
	defer outcome.Close()
	good, hits := countingServer(t)
	err = testClient(t, outcome.URL, good.URL).call(ctx, http.MethodPost, "/v1/registries", map[string]string{"host": "x"}, nil)
	if err == nil || unknown.Load() != 1 || hits.Load() != 0 {
		t.Fatalf("a write whose outcome is unknown: %v, sent %d times and to the next server %d times", err, unknown.Load(), hits.Load())
	}
}

// TestCLIPassesAStoppedServerAndSaysWhyNoServerLeads: with no leader, and
// a server whose process is stopped, which takes the TCP connection in its
// kernel and never answers the TLS handshake, a call gives that server 3 s,
// not the 5 s of its handshake timeout, and prints the other server's
// answer that no server leads, not the stopped one's timeout (the ninth
// soak's fourth finding).
func TestCLIPassesAStoppedServerAndSaysWhyNoServerLeads(t *testing.T) {
	isolate(t)
	wait := leaderWait
	leaderWait = 500 * time.Millisecond
	t.Cleanup(func() { leaderWait = wait })
	const why = "no leader: this server reaches 1 of the 3 servers, and a write needs 2"
	follower := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set(nodeapi.NoLeaderHeader, "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"` + why + `"}`))
	}))
	defer follower.Close()
	// Nothing accepts from this listener: a connect completes in the
	// backlog, and the handshake waits.
	stopped, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer stopped.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: follower.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := (&clientFlags{server: follower.URL + ",https://" + stopped.Addr().String(), token: "tok", caCert: ca}).client()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		start := time.Now()
		err := c.call(context.Background(), m, "/v1/registries", map[string]string{"host": "x"}, nil)
		if d := time.Since(start); err == nil || err.Error() != why || d > 4500*time.Millisecond {
			t.Fatalf("%s with no leader and a stopped server: %v after %s", m, err, d)
		}
	}
}
