package router

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// An endpoint whose dials keep failing is skipped for longer each time: 3 s,
// then 6, 12, 24, and 30 from then on. A failure while it is marked down
// does not lengthen the mark. A request that gets through ends the run.
func TestEndpointBackoff(t *testing.T) {
	e := newEndpoint("10.0.0.9:1", false, upstreamResponseTimeout)
	t0 := time.Unix(1000, 0)
	at := t0
	for i, want := range []time.Duration{3, 6, 12, 24, 30, 30} {
		want *= time.Second
		if got := e.failed(at); got != want {
			t.Fatalf("failure %d at %s: skipped for %s, want %s", i+1, at.Sub(t0), got, want)
		}
		if got := e.failed(at.Add(time.Second)); got != 0 {
			t.Fatalf("a failure 1 s into the mark of failure %d lengthened it by %s", i+1, got)
		}
		at = at.Add(want)
	}
	e.ok()
	if got := e.failed(at); got != downFor {
		t.Fatalf("the first failure after a request got through: %s, want %s", got, downFor)
	}
	if e.ok(); e.downUntil.Load() != 0 || e.fails.Load() != 0 {
		t.Fatal("ok left the endpoint marked down")
	}
}

// lockedClock is a clock a test moves, safe for the proxy's goroutines.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// While a replica's address stays dead, as while the server's machine was
// gone in the third soak, requests dial it less and less often: there, the
// workers' routers dialled the server's dead replicas every 3 s for the
// whole outage, and each dial held a live request for the 2 s timeout. A
// request every second for two minutes against one dead and one live
// endpoint dials the dead one 7 times (at 0, 3, 9, 21, 45, 75 and 105 s),
// not 30 or more, and every request gets its reply from the live one. An
// endpoint that leaves the table and comes back starts again at 3 s.
func TestADeadEndpointIsDialledLessAndLess(t *testing.T) {
	up := upstream(t, "up")
	dead := deadAddr(t)
	m := NewMetrics()
	p := newProxy(slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	p.httpsPort = 443
	clock := &lockedClock{t: time.Unix(1_700_000_000, 0)}
	p.now = clock.Now
	p.setTable(Table{"www.example.com": whole(dead, up.Listener.Addr().String())})

	get := func() int {
		req := httptest.NewRequest(http.MethodGet, "https://www.example.com/", nil)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	dials := func() uint64 {
		var n uint64
		for _, u := range m.report(nil).Upstream {
			if u.Kind == "dial" {
				n += u.Count
			}
		}
		return n
	}
	for s := range 120 {
		if code := get(); code != http.StatusOK {
			t.Fatalf("request at %d s: %d", s, code)
		}
		clock.Advance(time.Second)
	}
	if n := dials(); n < 6 || n > 8 {
		t.Fatalf("%d dials of the dead endpoint in 120 s of a request a second, want 7", n)
	}

	// The dead endpoint leaves the table and comes back: its first failure
	// skips it for 3 s again.
	p.setTable(Table{"www.example.com": whole(up.Listener.Addr().String())})
	p.setTable(Table{"www.example.com": whole(dead, up.Listener.Addr().String())})
	before := dials()
	for dials() == before {
		if code := get(); code != http.StatusOK {
			t.Fatalf("request after the endpoint came back: %d", code)
		}
	}
	p.mu.RLock()
	e := p.endpoints[epKey{addr: dead}]
	p.mu.RUnlock()
	if got := time.Duration(e.downUntil.Load() - clock.Now().UnixNano()); got != downFor {
		t.Fatalf("the endpoint that came back is skipped for %s, want %s", got, downFor)
	}
}
