package nodeapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// twoServers is the old server and the restored one, two servers of one CA
// with a certificate for one name, at epochs 0 and 1, and a dialer that
// sends that name to either.
type twoServers struct {
	ca            *pki.CA
	old, restored *countingServer
	target        atomic.Pointer[string] // the address the name leads to
}

// countingServer is a node API server that answers at its epoch, and
// counts its requests and its open connections.
type countingServer struct {
	ts       *httptest.Server
	epoch    atomic.Pointer[string]
	requests atomic.Int64
	open     atomic.Int64
}

func (s *countingServer) set(epoch string) { s.epoch.Store(&epoch) }

func (s *countingServer) addr() string { return s.ts.Listener.Addr().String() }

const twoServersName = "hivemind-server.test"

func newTwoServers(t *testing.T) (*twoServers, *pki.CA) {
	t.Helper()
	ca, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	two := &twoServers{ca: ca}
	two.old, two.restored = two.start(t, "0"), two.start(t, "1")
	return two, ca
}

// start starts another server behind the name, at epoch.
func (two *twoServers) start(t *testing.T, epoch string) *countingServer {
	t.Helper()
	s := &countingServer{}
	s.set(epoch)
	s.ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.Header().Set(EpochHeader, *s.epoch.Load())
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assignments":[]}`))
	}))
	s.ts.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			s.open.Add(1)
		case http.StateClosed, http.StateHijacked:
			s.open.Add(-1)
		}
	}
	s.ts.TLS = pki.ServerTLSConfig(two.ca, issueTLS(t, two.ca, "node-server00000", twoServersName))
	s.ts.StartTLS()
	t.Cleanup(s.ts.Close)
	return s
}

// lead makes the name lead to s, as a changed record or /etc/hosts line
// does.
func (two *twoServers) lead(s *countingServer) {
	a := s.ts.Listener.Addr().String()
	two.target.Store(&a)
}

// client returns a worker's client for the name, with g, whose transport
// dials where the name leads at each new connection.
func (two *twoServers) client(t *testing.T, ca *pki.CA, g *EpochGuard) *Client {
	t.Helper()
	tlsCfg, err := pki.ClientTLSConfig(ca, issueTLS(t, ca, "node-aaaaaaaaaaaa", ""), twoServersName)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClient("https://"+twoServersName+":4484", tlsCfg, WithEpochGuard(g))
	if err != nil {
		t.Fatal(err)
	}
	c.tr.cur.Load().DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, *two.target.Load())
	}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// A worker that has seen epoch 1 is sent to the old server, at epoch 0,
// and refuses it; then the name leads to the restored server again. The
// request that asks again after Retry must reach the restored server. In
// the fourth soak, the refused reply put its connection back in the pool,
// every later request went out on it, the name was never looked up again,
// and the worker refused the old server for as long as that ran (4 min 26 s,
// with its replicas running twice). The refused connection is closed, not
// pooled, both after a reply without a body, as the lease's 204, and after
// one with a body.
func TestEpochRefusalLooksTheNameUpAgain(t *testing.T) {
	for _, first := range []string{"lease", "assignments"} {
		t.Run(first, func(t *testing.T) {
			two, ca := newTwoServers(t)
			now := time.Unix(1_700_000_000, 0)
			g := NewEpochGuard(1)
			g.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
			g.Retry = 10 * time.Second
			g.Now = func() time.Time { return now }
			c := two.client(t, ca, g)
			ctx := context.Background()
			call := map[string]func() error{
				"lease":       func() error { return c.RenewLease(ctx, "node-aaaaaaaaaaaa") },
				"assignments": func() error { _, err := c.AssignmentsForNode(ctx, "node-aaaaaaaaaaaa"); return err },
			}

			two.lead(two.old)
			var stale *StaleServerError
			if err := call[first](); !errors.As(err, &stale) || stale.Epoch != 0 || stale.Seen != 1 {
				t.Fatalf("the old server: %v, want a refusal at epoch 0", err)
			}

			two.lead(two.restored)
			now = now.Add(g.Retry)
			if err := call["assignments"](); err != nil {
				t.Fatalf("one Retry after the name led to the restored server: %v; the old server had %d requests, the restored one %d",
					err, two.old.requests.Load(), two.restored.requests.Load())
			}
			if two.old.requests.Load() != 1 || two.restored.requests.Load() != 1 {
				t.Fatalf("requests: old %d, restored %d; want 1 each", two.old.requests.Load(), two.restored.requests.Load())
			}
			waitFor(t, "the refused connection to close", func() bool { return two.old.open.Load() == 0 })
			for name, f := range call {
				if err := f(); err != nil {
					t.Fatalf("%s on the restored server: %v", name, err)
				}
			}
		})
	}
}

// Two machines answer one name, the old server at epoch 0 and the restored
// one at 1, and the worker's replies come from each in turn, as from a
// round-robin record, or as in the fourth soak, where a long poll sent to
// the restored server before the name led to the old one came back between
// two refusals. The refusal is logged once, naming the old server's
// address; a reply from the restored server, which was never refused, logs
// nothing; a second old machine behind the name gets a refusal of its own;
// and "talks to it again" is logged only for an address that was refused,
// once it answers at the cluster's epoch. Before, the guard kept one state
// for the whole client, and logged both lines again at each switch.
func TestEpochGuardKeysTheRefusalByAddress(t *testing.T) {
	two, ca := newTwoServers(t)
	now := time.Unix(1_700_000_000, 0)
	logs := &syncBuf{}
	g := NewEpochGuard(1)
	g.Log = slog.New(slog.NewTextHandler(logs, nil))
	g.Retry = 10 * time.Second
	g.Now = func() time.Time { return now }
	c := two.client(t, ca, g)
	// get sends a request to s: the name leads to it, and the pooled
	// connection to the other machine, if any, is closed, so the request
	// dials. It is a Retry after the last one, so none is held off.
	get := func(s *countingServer) error {
		two.lead(s)
		c.CloseIdleConnections()
		now = now.Add(g.Retry)
		_, err := c.AssignmentsForNode(context.Background(), "node-aaaaaaaaaaaa")
		return err
	}
	server := twoServersName + ":4484"
	refused := func(s *countingServer, err error) bool {
		var stale *StaleServerError
		return errors.As(err, &stale) && stale.Server == server && stale.Addr == s.addr() && stale.Epoch == 0 && stale.Seen == 1
	}
	refusal := func(s *countingServer) string {
		return "the server at " + server + " (" + s.addr() + ") is from before a restore of this cluster"
	}
	for i := range 3 {
		if err := get(two.old); !refused(two.old, err) {
			t.Fatalf("round %d, the old server: %v, want a refusal from %s", i, err, two.old.addr())
		}
		if err := get(two.restored); err != nil {
			t.Fatalf("round %d, the restored server: %v", i, err)
		}
	}
	l := logs.String()
	if n := strings.Count(l, "level=ERROR"); n != 1 || !strings.Contains(l, refusal(two.old)) || !strings.Contains(l, "address="+two.old.addr()) {
		t.Fatalf("%d refusals logged, want one naming %s:\n%s", n, two.old.addr(), l)
	}
	if strings.Contains(l, "talks to it again") {
		t.Fatalf("a reply from the restored server, never refused, was logged as talking to a server again:\n%s", l)
	}
	if !strings.Contains((&StaleServerError{Server: server, Addr: two.old.addr()}).Error(), server+" ("+two.old.addr()+")") {
		t.Fatal("the error does not name the address")
	}

	// A second old machine behind the name.
	other := two.start(t, "0")
	for range 2 {
		if err := get(other); !refused(other, err) {
			t.Fatalf("a second old server: %v", err)
		}
		if err := get(two.old); !refused(two.old, err) {
			t.Fatalf("the first old server again: %v", err)
		}
	}
	l = logs.String()
	if n := strings.Count(l, "level=ERROR"); n != 2 || !strings.Contains(l, refusal(other)) {
		t.Fatalf("%d refusals logged, want one for each old server:\n%s", n, l)
	}

	// The first old server answers at the cluster's epoch now.
	two.old.set("1")
	for range 2 {
		if err := get(two.old); err != nil {
			t.Fatalf("the old server at epoch 1: %v", err)
		}
	}
	l = logs.String()
	again := "the server at " + server + " (" + two.old.addr() + ") answers at this cluster's epoch now; this worker talks to it again"
	if strings.Count(l, "talks to it again") != 1 || !strings.Contains(l, again) {
		t.Fatalf("want one line that the worker talks to %s again:\n%s", two.old.addr(), l)
	}
	if err := get(other); !refused(other, err) || strings.Count(logs.String(), "level=ERROR") != 2 {
		t.Fatalf("the second old server, still refused: %v, logs:\n%s", err, logs.String())
	}
}
