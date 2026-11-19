package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// wsOpen opens an upgraded connection through the router to the echo
// replica of www.example.com, and checks that it echoes.
func wsOpen(t *testing.T, tr *testRouter) io.ReadWriteCloser {
	t.Helper()
	ws := tr.client(true)
	ws.Timeout = 0
	resp := upgrade(t, ws, "www.example.com", "websocket")
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		t.Fatalf("upgrade: %d", resp.StatusCode)
	}
	rwc := resp.Body.(io.ReadWriteCloser)
	t.Cleanup(func() { rwc.Close() })
	buf := make([]byte, 4)
	if _, err := rwc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rwc, buf); err != nil {
		t.Fatalf("echo: %v", err)
	}
	return rwc
}

// closedSoon checks that rwc is closed within a second, by its other end.
func closedSoon(t *testing.T, rwc io.ReadWriteCloser) {
	t.Helper()
	start := time.Now()
	stop := time.AfterFunc(5*time.Second, func() { rwc.Close() })
	defer stop.Stop()
	if _, err := rwc.Read(make([]byte, 4)); err == nil {
		t.Fatal("read data from an upgraded connection after Shutdown")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the upgraded connection stayed open %v after Shutdown returned", took)
	}
}

// A router stopped with a request in flight and an upgraded connection
// waits for the request for its limit, then closes both connections, logs
// how many, and returns no error. A second Shutdown returns the same at
// once.
func TestShutdownClosesTheConnectionsLeftAfterItsWait(t *testing.T) {
	const wait = 300 * time.Millisecond
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(held) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(release)
		slow.Close()
	})
	tr := newTestRouterWith(t, Table{"www.example.com": whole(switcher(t, false)), "api.example.com": whole(slow.Listener.Addr().String())},
		testOptions{poll: time.Hour, before: func(rt *Router) { rt.shutdownWait = wait }})
	rwc := wsOpen(t, tr)
	inFlight := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/", nil)
		resp, err := tr.client(false).Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		inFlight <- err
	}()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the replica")
	}

	start := time.Now()
	err := tr.rt.Shutdown()
	took := time.Since(start)
	if err != nil {
		t.Fatalf("Shutdown with a request in flight at the end of its wait: %v, want nil", err)
	}
	if took < wait || took > wait+5*time.Second {
		t.Fatalf("Shutdown took %v, want about its wait of %v", took, wait)
	}
	closedSoon(t, rwc)
	select {
	case err := <-inFlight:
		if err == nil {
			t.Fatal("the request in flight was answered after Shutdown closed its connection")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request in flight still waits 5 s after Shutdown")
	}
	if n := tr.rt.limits.open(); n != 0 {
		t.Fatalf("%d connections open after Shutdown", n)
	}
	if logs := tr.logs.String(); !strings.Contains(logs, "level=INFO msg=\"router stopped; closed the connections still open after its wait for requests in flight\" connections=2 waited=") ||
		!strings.Contains(logs, "wait=300ms") {
		t.Fatalf("Shutdown did not log the 2 connections it closed:\n%s", logs)
	}
	start = time.Now()
	if err := tr.rt.Shutdown(); err != nil || time.Since(start) > time.Second {
		t.Fatalf("a second Shutdown: %v after %v", err, time.Since(start))
	}
}

// An upgraded connection does not hold Shutdown, which http.Server does not
// wait for: Shutdown returns at once, and closes it.
func TestShutdownClosesUpgradedConnections(t *testing.T) {
	tr := newTestRouterWith(t, Table{"www.example.com": whole(switcher(t, false))},
		testOptions{poll: time.Hour, before: func(rt *Router) { rt.shutdownWait = time.Minute }})
	rwc := wsOpen(t, tr)
	start := time.Now()
	if err := tr.rt.Shutdown(); err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("Shutdown with an upgraded connection: %v after %v", err, time.Since(start))
	}
	closedSoon(t, rwc)
	if !strings.Contains(tr.logs.String(), "closed the connections still open after its wait for requests in flight\" connections=1 ") {
		t.Fatalf("Shutdown did not log the connection it closed:\n%s", tr.logs.String())
	}
}

// A router whose connections are idle closes them as before, and logs
// nothing about it.
func TestShutdownWithIdleConnections(t *testing.T) {
	tr := newTestRouterWith(t, Table{"www.example.com": whole(upstream(t, "up").Listener.Addr().String())},
		testOptions{poll: time.Hour, before: func(rt *Router) { rt.shutdownWait = time.Minute }})
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, _ := get(t, tr.client(false), req); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	start := time.Now()
	if err := tr.rt.Shutdown(); err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("Shutdown with an idle connection: %v after %v", err, time.Since(start))
	}
	if strings.Contains(tr.logs.String(), "closed the connections still open") {
		t.Fatalf("Shutdown logged closing connections that were idle:\n%s", tr.logs.String())
	}
}
