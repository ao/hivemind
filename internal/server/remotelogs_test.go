package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

const workerNode = "node-2"

// fakeWorker plays a worker on the relay: it polls for workerNode, and answers
// each request with script, which writes frames to fw. It records when the
// server side aborted a stream.
type fakeWorker struct {
	relay  *logrelay.Relay
	script func(q nodeapi.LogRequest, fw *nodeapi.FrameWriter, aborted <-chan struct{})

	mu       sync.Mutex
	requests []nodeapi.LogRequest
	aborts   chan struct{}
}

func (f *fakeWorker) run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	for ctx.Err() == nil {
		q, err := f.relay.Next(ctx, workerNode)
		if err != nil {
			return
		}
		if q == nil {
			continue
		}
		f.mu.Lock()
		f.requests = append(f.requests, *q)
		f.mu.Unlock()
		pr, pw := io.Pipe()
		c, err := f.relay.Claim(workerNode, q.ID, pr)
		if err != nil {
			continue
		}
		aborted := make(chan struct{})
		var once sync.Once
		abort := func() {
			once.Do(func() {
				close(aborted)
				pw.CloseWithError(errors.New("aborted"))
				select {
				case f.aborts <- struct{}{}:
				default:
				}
			})
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Wait(ctx, abort)
		}()
		go func() {
			defer wg.Done()
			fw := nodeapi.NewFrameWriter(pw)
			f.script(*q, fw, aborted)
			pw.Close()
		}()
	}
}

func checkGoroutines(t *testing.T) {
	t.Helper()
	before := goruntime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for goruntime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:goruntime.Stack(buf, true)]
				t.Errorf("%d goroutines before, %d after:\n%s", before, goruntime.NumGoroutine(), buf)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// remoteHarness is a harness with a relay and a second node, workerNode, with
// app web's replica remote on it and replica local on the server's node.
type remoteHarness struct {
	*harness
	relay  *logrelay.Relay
	worker *fakeWorker
	remote int
	local  int
}

func newRemoteHarness(t *testing.T, o logrelay.Options, script func(nodeapi.LogRequest, *nodeapi.FrameWriter, <-chan struct{})) *remoteHarness {
	t.Helper()
	relay := logrelay.New(o)
	h := newHarness(t, func(c *Config) { c.Logs = relay })
	ctx := context.Background()
	if err := h.store.UpsertNode(ctx, store.Node{ID: workerNode, Name: "w", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)
	reps, err := h.store.Replicas(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	rh := &remoteHarness{harness: h, relay: relay, remote: -1, local: -1}
	for _, rp := range reps {
		switch rp.NodeID {
		case workerNode:
			rh.remote = rp.Index
		case testNode:
			rh.local = rp.Index
		}
	}
	if rh.remote < 0 || rh.local < 0 {
		t.Fatalf("replicas not spread over both nodes: %+v", reps)
	}
	if script != nil {
		rh.worker = &fakeWorker{relay: relay, script: script, aborts: make(chan struct{}, 8)}
		wctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			rh.worker.run(wctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
	}
	return rh
}

func (rh *remoteHarness) logs(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return rh.do(t, "GET", fmt.Sprintf("/v1/apps/web/logs?replica=%d%s", rh.remote, query), "")
}

func shortRelay() logrelay.Options {
	return logrelay.Options{PickupTimeout: 300 * time.Millisecond, PollTimeout: 200 * time.Millisecond}
}

func TestRemoteLogsAreReadThroughTheWorker(t *testing.T) {
	checkGoroutines(t)
	rh := newRemoteHarness(t, shortRelay(), func(q nodeapi.LogRequest, fw *nodeapi.FrameWriter, _ <-chan struct{}) {
		_ = fw.WriteFrame(nodeapi.FrameReady, nil)
		_ = fw.WriteFrame(nodeapi.FrameStdout, []byte("to stdout\n"))
		_ = fw.WriteFrame(nodeapi.FrameStderr, []byte("to stderr\n"))
	})
	rec := rh.logs(t, "&tail=7")
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "to stdout\nto stderr\n" {
		t.Fatalf("body %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type %q", ct)
	}
	rh.worker.mu.Lock()
	q := rh.worker.requests[0]
	rh.worker.mu.Unlock()
	if q.App != "web" || q.Replica != rh.remote || q.Tail != 7 || q.Follow {
		t.Fatalf("worker got %+v", q)
	}
	// The local replica is still read from the local runtime.
	rh.rt.containers = []runtime.Container{{ID: "loc", Name: "hivemind-web-x", State: "running",
		Labels: map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: fmt.Sprint(rh.local), runtime.LabelGeneration: "1"}}}
	rh.rt.logLines = []string{"local line"}
	rec = rh.do(t, "GET", fmt.Sprintf("/v1/apps/web/logs?replica=%d", rh.local), "")
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "local line\n" || rh.rt.lastID != "loc" {
		t.Fatalf("local body %q from %q", rec.Body.String(), rh.rt.lastID)
	}
}

func TestRemoteLogsErrorsFromTheWorker(t *testing.T) {
	cases := []struct {
		frame byte
		code  int
		want  string
	}{
		{nodeapi.FrameNotFound, http.StatusNotFound, "has no container on node " + workerNode},
		{nodeapi.FrameUnavailable, http.StatusServiceUnavailable, "container engine on node " + workerNode + " is unavailable"},
		{nodeapi.FrameFailed, http.StatusBadGateway, "could not read the log: boom"},
	}
	for _, c := range cases {
		rh := newRemoteHarness(t, shortRelay(), func(_ nodeapi.LogRequest, fw *nodeapi.FrameWriter, _ <-chan struct{}) {
			_ = fw.WriteFrame(c.frame, []byte("boom\x1b[31m"))
		})
		rec := rh.logs(t, "")
		wantStatus(t, rec, c.code)
		if got := errorOf(t, rec); !strings.Contains(got, c.want) || strings.Contains(got, "\x1b") {
			t.Fatalf("frame %d: error %q", c.frame, got)
		}
	}
}

func TestRemoteLogsWithNoWorkerTimesOut(t *testing.T) {
	checkGoroutines(t)
	rh := newRemoteHarness(t, shortRelay(), nil)
	start := time.Now()
	rec := rh.logs(t, "")
	wantStatus(t, rec, http.StatusGatewayTimeout)
	if got := errorOf(t, rec); !strings.Contains(got, "worker "+workerNode+" did not answer") || !strings.Contains(got, "is it running?") {
		t.Fatalf("error %q", got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestRemoteLogsWorkerThatNeverStartsTimesOut(t *testing.T) {
	checkGoroutines(t)
	rh := newRemoteHarness(t, shortRelay(), func(_ nodeapi.LogRequest, _ *nodeapi.FrameWriter, aborted <-chan struct{}) {
		<-aborted // claims the request, then sends nothing
	})
	rec := rh.logs(t, "")
	wantStatus(t, rec, http.StatusGatewayTimeout)
	if got := errorOf(t, rec); !strings.Contains(got, "did not answer") {
		t.Fatalf("error %q", got)
	}
}

func TestRemoteLogsOfALostNode(t *testing.T) {
	rh := newRemoteHarness(t, shortRelay(), nil)
	timeNow = func() time.Time { return time.Now().Add(time.Minute) }
	t.Cleanup(func() { timeNow = time.Now })
	start := time.Now()
	rec := rh.logs(t, "")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if got := errorOf(t, rec); !strings.Contains(got, "node "+workerNode+", which is lost") {
		t.Fatalf("error %q", got)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("a lost node's request waited for the pickup timeout")
	}
}

func TestRemoteLogsWithoutTheClusterListener(t *testing.T) {
	h := newHarness(t)
	if err := h.store.UpsertNode(context.Background(), store.Node{ID: workerNode, Name: "w", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, h.do(t, "PUT", "/v1/apps/web", nginx2), http.StatusCreated)
	reps, _ := h.store.Replicas(context.Background(), "web")
	for _, rp := range reps {
		if rp.NodeID != workerNode {
			continue
		}
		rec := h.do(t, "GET", fmt.Sprintf("/v1/apps/web/logs?replica=%d", rp.Index), "")
		wantStatus(t, rec, http.StatusServiceUnavailable)
		if got := errorOf(t, rec); !strings.Contains(got, "without --advertise") {
			t.Fatalf("error %q", got)
		}
		return
	}
	t.Fatal("no replica on the worker")
}

// TestRemoteLogsFollowAndDisconnect runs a real HTTP server: a followed stream
// delivers lines as the worker sends them, past the write timeout, and when
// the user disconnects the worker's stream is aborted at once.
func TestRemoteLogsFollowAndDisconnect(t *testing.T) {
	checkGoroutines(t)
	lines := make(chan string)
	rh := newRemoteHarness(t, shortRelay(), func(_ nodeapi.LogRequest, fw *nodeapi.FrameWriter, aborted <-chan struct{}) {
		_ = fw.WriteFrame(nodeapi.FrameReady, nil)
		for {
			select {
			case <-aborted:
				return
			case l := <-lines:
				_ = fw.WriteFrame(nodeapi.FrameStdout, []byte(l+"\n"))
			}
		}
	})
	ts := httptest.NewUnstartedServer(rh.srv.Handler())
	ts.Config = NewHTTPServer(rh.srv.Handler(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts.Config.WriteTimeout = 300 * time.Millisecond
	ts.Start()
	t.Cleanup(ts.Close)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/v1/apps/web/logs?replica=%d&follow=true", ts.URL, rh.remote), nil)
	req.Header.Set("Authorization", "Bearer "+rh.token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	for i := range 4 {
		lines <- fmt.Sprintf("line %d", i)
		if !sc.Scan() || sc.Text() != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d: %q %v", i, sc.Text(), sc.Err())
		}
		time.Sleep(120 * time.Millisecond) // four of these outlast the write timeout
	}
	cancel()
	select {
	case <-rh.worker.aborts:
	case <-time.After(2 * time.Second):
		t.Fatal("the worker's stream was not aborted after the user left")
	}
}

func TestRemoteLogsCloseStreamsEndsThem(t *testing.T) {
	checkGoroutines(t)
	rh := newRemoteHarness(t, shortRelay(), func(_ nodeapi.LogRequest, fw *nodeapi.FrameWriter, aborted <-chan struct{}) {
		_ = fw.WriteFrame(nodeapi.FrameReady, nil)
		<-aborted
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rh.logs(t, "&follow=true") }()
	time.Sleep(200 * time.Millisecond)
	rh.srv.CloseStreams()
	select {
	case rec := <-done:
		wantStatus(t, rec, http.StatusOK)
	case <-time.After(3 * time.Second):
		t.Fatal("CloseStreams did not end a relayed stream")
	}
}

func TestRemoteLogsCapPerNode(t *testing.T) {
	checkGoroutines(t)
	o := shortRelay()
	o.MaxStreamsPerNode = 1
	rh := newRemoteHarness(t, o, func(q nodeapi.LogRequest, fw *nodeapi.FrameWriter, aborted <-chan struct{}) {
		_ = fw.WriteFrame(nodeapi.FrameReady, nil)
		if q.Follow {
			<-aborted
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan int, 1)
	go func() {
		req := httptest.NewRequestWithContext(ctx, "GET", fmt.Sprintf("/v1/apps/web/logs?replica=%d&follow=true", rh.remote), nil)
		req.Header.Set("Authorization", "Bearer "+rh.token)
		rec := httptest.NewRecorder()
		rh.srv.Handler().ServeHTTP(rec, req)
		first <- rec.Code
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rh.worker.mu.Lock()
		n := len(rh.worker.requests)
		rh.worker.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first stream did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec := rh.logs(t, "")
	wantStatus(t, rec, http.StatusTooManyRequests)
	if got := errorOf(t, rec); !strings.Contains(got, "already has 1 log streams open") {
		t.Fatalf("error %q", got)
	}
	cancel()
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first stream %d", code)
	}
	// The slot is free once the first stream has ended on both sides.
	deadline = time.Now().Add(2 * time.Second)
	for {
		rec = rh.logs(t, "&tail=1")
		if rec.Code != http.StatusTooManyRequests || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rec.Code == http.StatusTooManyRequests {
		t.Fatal("the slot was not freed")
	}
}
