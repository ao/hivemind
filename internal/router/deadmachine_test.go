package router

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The timeouts of a machine that lost power, scaled down: a connection that
// sent data fails after testUserTimeout, as TCP_USER_TIMEOUT ends it, and a
// dial after testDialTimeout.
const (
	testUserTimeout = time.Second
	testDialTimeout = 200 * time.Millisecond
)

// machine is one fake replica machine. Its connections go to a real
// upstream until it loses power (off); after that, what is written on them
// goes nowhere, a read once something was written fails with ETIMEDOUT
// testUserTimeout after the write, and a dial fails after testDialTimeout.
type machine struct{ off atomic.Bool }

type blackholeConn struct {
	net.Conn
	m      *machine
	closed atomic.Bool
	mu     sync.Mutex
	wrote  time.Time // the first write after the power-off
}

func (c *blackholeConn) Write(b []byte) (int, error) {
	if c.m.off.Load() {
		c.mu.Lock()
		if c.wrote.IsZero() {
			c.wrote = time.Now()
		}
		c.mu.Unlock()
		return len(b), nil
	}
	return c.Conn.Write(b)
}

func (c *blackholeConn) Read(b []byte) (int, error) {
	for {
		if c.closed.Load() {
			return 0, net.ErrClosed
		}
		if c.m.off.Load() {
			c.mu.Lock()
			w := c.wrote
			c.mu.Unlock()
			if w.IsZero() {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if d := time.Until(w.Add(testUserTimeout)); d > 0 {
				time.Sleep(d)
			}
			return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ETIMEDOUT)}
		}
		// Short reads, so the power-off is seen while the transport waits
		// on an idle connection.
		_ = c.Conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		n, err := c.Conn.Read(b)
		if errors.Is(err, os.ErrDeadlineExceeded) && n == 0 {
			continue
		}
		return n, err
	}
}

func (c *blackholeConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// deadMachines is a router whose endpoints are on fake machines, by host,
// each connection going to one real upstream.
func deadMachines(t *testing.T, table Table, hosts ...string) (*testRouter, map[string]*machine) {
	t.Helper()
	up := upstream(t, "up").Listener.Addr().String()
	ms := map[string]*machine{}
	for _, h := range hosts {
		ms[h] = &machine{}
	}
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		m := ms[host]
		if m == nil {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("no such test machine")}
		}
		if m.off.Load() {
			select {
			case <-time.After(testDialTimeout):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.ETIMEDOUT)}
		}
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", up)
		if err != nil {
			return nil, err
		}
		return &blackholeConn{Conn: c, m: m}, nil
	}
	tr := newTestRouterWith(t, table, testOptions{poll: time.Hour, before: func(rt *Router) {
		rt.proxy.dial = dial
		rt.proxy.userTimeout = testUserTimeout
	}})
	return tr, ms
}

// timedGet sends one GET for www.example.com and returns its status and how
// long it took.
func timedGet(t *testing.T, c *http.Client) (int, time.Duration) {
	t.Helper()
	began := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	resp, _, _ := get(t, c, req)
	return resp.StatusCode, time.Since(began)
}

// warm sends n GETs, one to each endpoint in turn, so each has a pooled
// connection.
func warm(t *testing.T, c *http.Client, n int) {
	t.Helper()
	for i := range n {
		if code, _ := timedGet(t, c); code != http.StatusOK {
			t.Fatalf("GET %d with every machine up: %d", i, code)
		}
	}
}

// Nine endpoints on five machines; two of them lose power, one with two
// replicas next to each other in the route's order. The GET that starts at
// the first of those two fails on its pooled connection at the user timeout,
// marks both of that machine's endpoints down, skips the second, and is
// answered by the next machine: one user timeout, not two. The GETs after it
// go to the machines that are up.
func TestAMachineWhoseConnectionTimedOutIsSkipped(t *testing.T) {
	eps := []string{"10.9.0.10:1", "10.9.0.10:2", "10.9.0.1:1", "10.9.0.2:1", "10.9.0.3:1", "10.9.0.20:1", "10.9.0.1:2", "10.9.0.2:2", "10.9.0.3:2"}
	tr, ms := deadMachines(t, Table{"www.example.com": whole(eps...)}, "10.9.0.1", "10.9.0.2", "10.9.0.3", "10.9.0.10", "10.9.0.20")
	c := tr.client(true)
	warm(t, c, len(eps))
	ms["10.9.0.10"].off.Store(true)
	ms["10.9.0.20"].off.Store(true)
	code, took := timedGet(t, c)
	if code != http.StatusOK || took > testUserTimeout*3/2 {
		t.Fatalf("the GET that starts at the dead machine's first endpoint: %d after %v, want 200 after one user timeout (%v)", code, took, testUserTimeout)
	}
	t.Logf("the GET that met the dead machine first: %d after %v", code, took)
	for i := range len(eps) - 1 {
		if code, took := timedGet(t, c); code != http.StatusOK || took > testUserTimeout*3/2 {
			t.Fatalf("GET %d after the power-off: %d after %v", i, code, took)
		}
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_upstream_errors_total{host="www.example.com",kind="connection"}`) {
		t.Fatalf("no connection error counted:\n%s", m)
	}
}

// Two machines next to each other in a route's order lose power: the GET
// that meets the first on its pooled connection fails at the user timeout,
// then tries the second on a new connection, which fails at the dial timeout
// rather than at another user timeout, and is answered by the third.
func TestAfterATimeoutTheNextTryDialsAnew(t *testing.T) {
	eps := []string{"10.9.0.10:1", "10.9.0.20:1", "10.9.0.1:1", "10.9.0.2:1"}
	tr, ms := deadMachines(t, Table{"www.example.com": whole(eps...)}, "10.9.0.1", "10.9.0.2", "10.9.0.10", "10.9.0.20")
	c := tr.client(true)
	warm(t, c, len(eps))
	ms["10.9.0.10"].off.Store(true)
	ms["10.9.0.20"].off.Store(true)
	code, took := timedGet(t, c)
	if code != http.StatusOK || took > testUserTimeout+testDialTimeout+testUserTimeout/2 {
		t.Fatalf("the GET that meets two dead machines in a row: %d after %v, want 200 after a user timeout and a dial timeout (%v)",
			code, took, testUserTimeout+testDialTimeout)
	}
	t.Logf("the GET that met two dead machines in a row: %d after %v", code, took)
}
