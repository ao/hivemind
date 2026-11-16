package nodeapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// response builds the reply statusError reads.
func response(code int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: header,
		Body:    io.NopCloser(strings.NewReader(body)),
		Request: &http.Request{Method: http.MethodGet, URL: &url.URL{Path: LogRequestsPath("node-a")}}}
}

// A 404 is "not registered" only when the server says so: with the mark,
// or as a server older than 2.2 does, with a JSON error and no version. A
// 404 from http.ServeMux is a route the server lacks.
func TestStatusErrorTellsAMissingRouteFromAMissingNode(t *testing.T) {
	marked := http.Header{ErrorHeader: {ErrorNotRegistered}, VersionHeader: {"v2.2.0"}}
	newer := http.Header{VersionHeader: {"v2.2.0"}}
	notRegistered := `{"error":"this node is not registered"}`
	cases := []struct {
		name string
		resp *http.Response
		want error
	}{
		{"marked", response(404, marked, notRegistered), ErrNotRegistered},
		{"2.0 server, node unknown", response(404, nil, notRegistered), ErrNotRegistered},
		{"2.0 server, no route", response(404, nil, "404 page not found\n"), ErrNoRoute},
		{"2.2 server, no route", response(404, newer, "404 page not found\n"), ErrNoRoute},
	}
	for _, c := range cases {
		if err := statusError(c.resp); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// A handler's own 404 on a server that marks its 404s is neither.
	err := statusError(response(404, newer, `{"error":"no such thing"}`))
	var refused *RefusedError
	if !errors.As(err, &refused) || errors.Is(err, ErrNotRegistered) || errors.Is(err, ErrNoRoute) || !strings.Contains(err.Error(), "no such thing") {
		t.Errorf("an unmarked JSON 404 from a 2.2 server: %v", err)
	}
	if err := statusError(response(404, nil, "404 page not found\n")); !strings.Contains(err.Error(), "/logs/requests") {
		t.Errorf("the error does not name the route: %v", err)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fakeWatcher struct {
	mu      sync.Mutex
	v       string
	changed chan struct{}
}

func (f *fakeWatcher) ServerVersion() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

func (f *fakeWatcher) ServerChanged() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeWatcher) set(v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.v = v
	close(f.changed)
	f.changed = make(chan struct{})
}

// A missing route is held off until the server changes or Retry passes, and
// logged once for each version of the server.
func TestMissingRoute(t *testing.T) {
	logs := &syncBuf{}
	w := &fakeWatcher{changed: make(chan struct{})}
	m := NewMissingRoute(w, "log streaming", slog.New(slog.NewTextHandler(logs, nil)))
	m.Retry = time.Hour
	if m.Skip() {
		t.Fatal("Skip before any call")
	}
	for range 3 {
		if err := m.Check(fmt.Errorf("%w (GET /x)", ErrNoRoute)); !errors.Is(err, ErrNoRoute) {
			t.Fatalf("Check changed the error: %v", err)
		}
	}
	if !m.Skip() {
		t.Fatal("no Skip after ErrNoRoute")
	}
	if n := strings.Count(logs.String(), "the server is older than this worker and has no log streaming; upgrade the server"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, logs)
	}
	if !strings.Contains(logs.String(), "server_version=\"none (older than 2.2)\"") {
		t.Errorf("the line does not say the server sends no version:\n%s", logs)
	}
	// An upgrade of the server lets the call through at once.
	w.set("v2.2.0")
	if m.Skip() {
		t.Fatal("Skip after the server changed")
	}
	m.Check(ErrNoRoute)
	if n := strings.Count(logs.String(), "has no log streaming"); n != 2 {
		t.Fatalf("a new server version is logged again: %d lines", n)
	}
	m.Check(nil)
	if m.Skip() {
		t.Fatal("Skip after a call that worked")
	}
	// Without a change, Retry lets one call through.
	m.Retry = 20 * time.Millisecond
	m.Check(ErrNoRoute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Wait(ctx)
	if ctx.Err() != nil || m.Skip() {
		t.Fatal("Retry did not let a call through")
	}
	var nilRoute *MissingRoute
	if nilRoute.Skip() || nilRoute.Check(ErrNoRoute) != ErrNoRoute {
		t.Fatal("a nil MissingRoute holds something off")
	}
}

// A worker newer than its server polls a server without log streaming once,
// logs it once, and polls again when another reply shows the server was
// upgraded, not every 30 s.
func TestLogServerWaitsForAnOlderServer(t *testing.T) {
	var mu sync.Mutex
	upgraded, polls := false, 0
	state := func() (bool, int) {
		mu.Lock()
		defer mu.Unlock()
		return upgraded, polls
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Prefix+"/nodes/{id}/lease", func(w http.ResponseWriter, r *http.Request) {
		if up, _ := state(); up {
			w.Header().Set(VersionHeader, "v2.2.0")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET "+Prefix+"/nodes/{id}/logs/requests", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		polls++
		up := upgraded
		mu.Unlock()
		if !up {
			http.NotFound(w, r) // what a server without the route answers
			return
		}
		w.Header().Set(VersionHeader, "v2.2.0")
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	c, err := NewClient(ts.URL, ts.Client().Transport.(*http.Transport).TLSClientConfig, WithVersion("v2.2.0"))
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuf{}
	ls := &LogServer{Client: c, NodeID: "node-a", Logger: slog.New(slog.NewTextHandler(logs, nil)), OldServerRetry: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ls.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	waitFor(t, "the first poll", func() bool { _, n := state(); return n >= 1 })
	time.Sleep(300 * time.Millisecond)
	if _, n := state(); n != 1 {
		t.Fatalf("%d polls of a server without the route, want 1", n)
	}
	if n := strings.Count(logs.String(), "has no log streaming; upgrade the server"); n != 1 || strings.Contains(logs.String(), "not registered") {
		t.Fatalf("logged %d times, want once, and not as unregistered:\n%s", n, logs)
	}

	mu.Lock()
	upgraded = true
	mu.Unlock()
	if err := c.RenewLease(context.Background(), "node-a"); err != nil {
		t.Fatal(err)
	}
	if v := c.ServerVersion(); v != "v2.2.0" {
		t.Fatalf("ServerVersion %q after a reply from the upgraded server", v)
	}
	waitFor(t, "a poll after the upgrade", func() bool { _, n := state(); return n >= 3 })
	if strings.Count(logs.String(), "level=WARN") != 1 {
		t.Fatalf("more warnings after the upgrade:\n%s", logs)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
