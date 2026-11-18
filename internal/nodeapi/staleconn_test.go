package nodeapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// twoServers is the old server and the restored one, two servers of one CA
// with a certificate for one name, at epochs 0 and 1, and a dialer that
// sends that name to either.
type twoServers struct {
	old, restored *countingServer
	target        atomic.Pointer[string] // the address the name leads to
}

// countingServer is a node API server that answers at one epoch, and
// counts its requests and its open connections.
type countingServer struct {
	ts       *httptest.Server
	requests atomic.Int64
	open     atomic.Int64
}

const twoServersName = "hivemind-server.test"

func newTwoServers(t *testing.T) (*twoServers, *pki.CA) {
	t.Helper()
	ca, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	start := func(epoch string) *countingServer {
		s := &countingServer{}
		s.ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.requests.Add(1)
			w.Header().Set(EpochHeader, epoch)
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
		s.ts.TLS = pki.ServerTLSConfig(ca, issueTLS(t, ca, "node-server00000", twoServersName))
		s.ts.StartTLS()
		t.Cleanup(s.ts.Close)
		return s
	}
	return &twoServers{old: start("0"), restored: start("1")}, ca
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
