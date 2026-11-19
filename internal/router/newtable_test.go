package router

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// A request that no replica of its route takes reads the table again, if
// the router's is staleTable old, and tries the replicas of the new table
// that it has not tried, its body too; with a younger table it gets 502 at
// once and reads nothing. Requests that come while a read waits to start,
// under refresh's limit of one a second, wait for that read.
func TestARequestNoReplicaTookTriesANewerTable(t *testing.T) {
	dead, up := deadAddr(t), upstream(t, "up").Listener.Addr().String()
	tr := newTestRouterPoll(t, Table{"www.example.com": whole(dead)}, time.Hour)
	waitFor(t, func() bool { return tr.readCount() >= 2 }) // the start's read and the poll's first one
	c := tr.client(true)
	p := tr.rt.proxy
	// The store has the replica that would take the request; the router's
	// table does not yet.
	tr.setRoutes(Table{"www.example.com": whole(dead, up)}, nil)

	p.tableAt.Store(time.Now().Add(time.Hour).UnixNano())
	before := tr.readCount()
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("every replica down, with a table just read: %d %q, want 502", resp.StatusCode, body)
	}
	if n := tr.readCount() - before; n != 0 {
		t.Fatalf("with a table just read, the request read it %d times", n)
	}

	p.tableAt.Store(time.Now().Add(-staleTable).UnixNano())
	req, _ = http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, s, body := get(t, c, req); resp.StatusCode != http.StatusOK || s.Name != "up" {
		t.Fatalf("every replica in the router's table down, a live one in the store's: %d %q, want 200 from up", resp.StatusCode, body)
	}
	if n := tr.readCount() - before; n != 1 {
		t.Fatalf("the request read the table %d times, want 1", n)
	}

	// A body none of which went to the dead replica goes to the new one.
	tr.rt.SetTable(Table{"www.example.com": whole(dead)})
	p.tableAt.Store(time.Now().Add(-staleTable).UnixNano())
	payload := strings.Repeat("b", 64<<10)
	req, _ = http.NewRequest(http.MethodPost, "https://www.example.com/", strings.NewReader(payload))
	if resp, s, body := get(t, c, req); resp.StatusCode != http.StatusOK || s.Name != "up" || s.Body != len(payload) {
		t.Fatalf("a POST with every replica in the router's table down: %d %q, want 200 from up with the whole body", resp.StatusCode, body)
	}

	// Every replica down in the store's table too: 502, and the requests
	// that came together shared their reads.
	down := Table{"www.example.com": whole(dead)}
	tr.setRoutes(down, nil)
	tr.rt.SetTable(down)
	p.tableAt.Store(time.Now().Add(-staleTable).UnixNano())
	before = tr.readCount()
	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
			resp, err := c.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		})
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusBadGateway {
			t.Fatalf("a request with every replica down in both tables: %d, want 502", code)
		}
	}
	if reads := tr.readCount() - before; reads < 1 || reads > 2 {
		t.Fatalf("%d requests together read the table %d times, want 1 or 2", n, reads)
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_requests_total{host="www.example.com",code="502"} 9`) {
		t.Fatalf("requests:\n%s", m)
	}
}

// On the server's router at its start, until its own node has reported
// (Config.Whole) and the table has been read again, a request whose route
// has replicas that do not take it, or no replica at all, waits for that
// table, and goes to the replica it brings, rather than get 502 or 503.
// After it, a request that no replica takes gets 502 at once.
func TestARouterWaitsForTheWholeTable(t *testing.T) {
	dead, up := deadAddr(t), upstream(t, "up").Listener.Addr().String()
	reported := make(chan struct{})
	tr := newTestRouterWith(t, Table{"www.example.com": whole(dead), "api.example.com": whole()}, testOptions{poll: time.Hour,
		config: func(c *Config) { c.Whole = reported },
		before: func(rt *Router) { rt.wholeWait = time.Minute }})
	c := tr.client(true)
	type result struct {
		host, name, err string
		code            int
	}
	results := make(chan result, 2)
	for _, host := range []string{"www.example.com", "api.example.com"} {
		go func() {
			req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
			resp, err := c.Do(req)
			if err != nil {
				results <- result{host: host, err: err.Error()}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var s seen
			_ = json.NewDecoder(bytes.NewReader(b)).Decode(&s)
			results <- result{host: host, name: s.Name, code: resp.StatusCode}
		}()
	}
	select {
	case r := <-results:
		t.Fatalf("a request was answered before the table had this node's replicas: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	tr.setRoutes(Table{"www.example.com": whole(dead, up), "api.example.com": whole(up)}, nil)
	close(reported)
	for range 2 {
		select {
		case r := <-results:
			if r.code != http.StatusOK || r.name != "up" {
				t.Fatalf("a request that waited for the whole table: %+v, want 200 from up", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a request still waits 5 s after the table had this node's replicas")
		}
	}

	tr.rt.SetTable(Table{"www.example.com": whole(dead), "api.example.com": whole()})
	start := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("every replica down after the wait: %d %q, want 502", resp.StatusCode, body)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://api.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no replica after the wait: %d %q, want 503", resp.StatusCode, body)
	}
	if took := time.Since(start); took > staleTable {
		t.Fatalf("the 502 and the 503 after the wait took %v", took)
	}
}

// A router whose table never becomes whole stops waiting for it WholeWait
// after it started, and says so.
func TestTheWaitForTheWholeTableEnds(t *testing.T) {
	const wait = 300 * time.Millisecond
	start := time.Now()
	tr := newTestRouterWith(t, Table{"www.example.com": whole(deadAddr(t)), "api.example.com": whole()}, testOptions{poll: time.Hour,
		config: func(c *Config) { c.Whole = make(chan struct{}) },
		before: func(rt *Router) { rt.wholeWait = wait }})
	c := tr.client(true)
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("every replica down: %d %q, want 502", resp.StatusCode, body)
	}
	if took := time.Since(start); took < wait {
		t.Fatalf("the 502 came %v after the start, before the wait of %v ended", took, wait)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://api.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no replica: %d %q, want 503", resp.StatusCode, body)
	}
	if !strings.Contains(tr.logs.String(), "requests no longer wait for them") {
		t.Fatalf("the end of the wait was not logged:\n%s", tr.logs.String())
	}
}
