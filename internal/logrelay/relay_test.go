package logrelay

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
)

// checkNoLeak fails the test if, after the test's cleanups, more goroutines
// run than before it started. Goroutines take a moment to exit, so it retries.
func checkNoLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			n := runtime.NumGoroutine()
			if n <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("%d goroutines before, %d after:\n%s", before, n, buf)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func quick() Options {
	return Options{PickupTimeout: 300 * time.Millisecond, PollTimeout: 200 * time.Millisecond,
		MaxStreamsPerNode: 2, MaxPollsPerNode: 1}
}

type openResult struct {
	st  *Stream
	err error
}

func openAsync(r *Relay, ctx context.Context, node string, q nodeapi.LogRequest) chan openResult {
	c := make(chan openResult, 1)
	go func() {
		st, err := r.Open(ctx, node, q)
		c <- openResult{st, err}
	}()
	return c
}

func mustNext(t *testing.T, r *Relay, node string) *nodeapi.LogRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q, err := r.Next(context.Background(), node)
		if err != nil {
			t.Fatal(err)
		}
		if q != nil {
			return q
		}
	}
	t.Fatal("no request reached the poll")
	return nil
}

// serveClaim runs the node side of a claim with body, the way the cluster
// handler does, and closes done when Wait returns.
func serveClaim(t *testing.T, r *Relay, node, id string, body io.Reader, abort func()) chan struct{} {
	t.Helper()
	c, err := r.Claim(node, id, body)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Wait(context.Background(), abort)
	}()
	return done
}

func TestOpenPollClaimRead(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web", Replica: 2, Tail: 5, Follow: true})
	q := mustNext(t, r, "node-a")
	if q.App != "web" || q.Replica != 2 || q.Tail != 5 || !q.Follow || !nodeapi.ValidLogRequestID(q.ID) {
		t.Fatalf("request %+v", q)
	}
	done := serveClaim(t, r, "node-a", q.ID, strings.NewReader("hello"), func() {})
	got := <-res
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.st.Request().ID != q.ID {
		t.Fatalf("stream for %q, want %q", got.st.Request().ID, q.ID)
	}
	b, err := io.ReadAll(got.st)
	if err != nil || string(b) != "hello" {
		t.Fatalf("read %q, %v", b, err)
	}
	select {
	case <-done:
		t.Fatal("the node side returned before the API side closed the stream")
	case <-time.After(50 * time.Millisecond):
	}
	got.st.Close()
	<-done
	if _, err := got.st.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after close succeeded")
	}
	// The request is gone once it is closed.
	if _, err := r.Claim("node-a", q.ID, strings.NewReader("")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("claim after close: %v", err)
	}
}

func TestRequestsQueueInOrderPerNode(t *testing.T) {
	checkNoLeak(t)
	o := quick()
	o.MaxStreamsPerNode = 4
	r := New(o)
	defer r.Close()
	a := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "one"})
	time.Sleep(20 * time.Millisecond)
	b := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "two"})
	c := openAsync(r, context.Background(), "node-b", nodeapi.LogRequest{App: "other"})
	time.Sleep(20 * time.Millisecond)
	q1, q2 := mustNext(t, r, "node-a"), mustNext(t, r, "node-a")
	if q1.App != "one" || q2.App != "two" {
		t.Fatalf("order %q, %q", q1.App, q2.App)
	}
	// node-a has nothing more; node-b's request is not handed to it.
	if q, err := r.Next(context.Background(), "node-a"); err != nil || q != nil {
		t.Fatalf("node-a got %+v, %v", q, err)
	}
	if q := mustNext(t, r, "node-b"); q.App != "other" {
		t.Fatalf("node-b got %q", q.App)
	}
	for _, ch := range []chan openResult{a, b, c} {
		if res := <-ch; !errors.Is(res.err, ErrNoAnswer) {
			t.Fatalf("unclaimed request: %v", res.err)
		}
	}
}

func TestPollWakesWhenARequestArrives(t *testing.T) {
	checkNoLeak(t)
	o := quick()
	o.PollTimeout = 5 * time.Second
	r := New(o)
	defer r.Close()
	got := make(chan *nodeapi.LogRequest, 1)
	go func() {
		q, _ := r.Next(context.Background(), "node-a")
		got <- q
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	q := <-got
	if q == nil || time.Since(start) > time.Second {
		t.Fatalf("poll returned %+v after %s", q, time.Since(start))
	}
	<-res
}

func TestUnansweredRequestTimesOut(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	start := time.Now()
	_, err := r.Open(context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 2*time.Second {
		t.Fatalf("timed out after %s", d)
	}
	// The slot is free again, and the request is not handed to a late poll.
	if q, err := r.Next(context.Background(), "node-a"); q != nil || err != nil {
		t.Fatalf("late poll got %+v, %v", q, err)
	}
	if r.nodes["node-a"].active != 0 || len(r.reqs) != 0 {
		t.Fatalf("state left behind: active %d, %d requests", r.nodes["node-a"].active, len(r.reqs))
	}
}

func TestPickedButUnclaimedTimesOutAndLateClaimIsRefused(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	q := mustNext(t, r, "node-a")
	if got := <-res; !errors.Is(got.err, ErrNoAnswer) {
		t.Fatalf("err %v", got.err)
	}
	if _, err := r.Claim("node-a", q.ID, strings.NewReader("")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("late claim: %v", err)
	}
}

func TestCancelledUserEndsTheWait(t *testing.T) {
	checkNoLeak(t)
	o := quick()
	o.PickupTimeout = 10 * time.Second
	r := New(o)
	defer r.Close()
	ctx, cancel := context.WithCancel(context.Background())
	res := openAsync(r, ctx, "node-a", nodeapi.LogRequest{App: "web"})
	time.Sleep(20 * time.Millisecond)
	cancel()
	if got := <-res; !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err %v", got.err)
	}
	if r.nodes["node-a"].active != 0 {
		t.Fatal("cancelled request still counts")
	}
}

// TestCancelCutsABlockedStream is a user who goes away while the worker sends
// nothing: Cancel makes the node side abort, the read fails, and both sides
// return.
func TestCancelCutsABlockedStream(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web", Follow: true})
	q := mustNext(t, r, "node-a")
	pr, pw := io.Pipe()
	aborted := make(chan struct{})
	var once sync.Once
	done := serveClaim(t, r, "node-a", q.ID, pr, func() {
		once.Do(func() { close(aborted) })
		pw.CloseWithError(errors.New("aborted"))
	})
	got := <-res
	if got.err != nil {
		t.Fatal(got.err)
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(got.st)
		readErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	got.st.Cancel()
	select {
	case <-aborted:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not abort the node side")
	}
	if err := <-readErr; err == nil {
		t.Fatal("read of a cut stream ended without an error")
	}
	got.st.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the node side did not return")
	}
	if r.nodes["node-a"].active != 0 {
		t.Fatal("ended stream still counts")
	}
}

// TestNodeGoingAwayAbortsToo: the node side's context ends (the worker went
// away, or its handler was cut), and the API side's read fails.
func TestNodeGoingAwayAbortsToo(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	q := mustNext(t, r, "node-a")
	pr, pw := io.Pipe()
	c, err := r.Claim("node-a", q.ID, pr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Wait(ctx, func() { pw.CloseWithError(errors.New("aborted")) })
	}()
	got := <-res
	cancel()
	if _, err := io.ReadAll(got.st); err == nil {
		t.Fatal("read ended without an error")
	}
	got.st.Close()
	<-done
}

func TestWrongNodeClaimIsRefusedAndLeavesTheRequest(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	q := mustNext(t, r, "node-a")
	if _, err := r.Claim("node-b", q.ID, strings.NewReader("forged")); !errors.Is(err, ErrWrongNode) {
		t.Fatalf("claim by another node: %v", err)
	}
	// node-b cannot poll node-a's requests either.
	if q2, err := r.Next(context.Background(), "node-b"); q2 != nil || err != nil {
		t.Fatalf("node-b polled %+v, %v", q2, err)
	}
	done := serveClaim(t, r, "node-a", q.ID, strings.NewReader("real"), func() {})
	got := <-res
	if got.err != nil {
		t.Fatal(got.err)
	}
	b, _ := io.ReadAll(got.st)
	if string(b) != "real" {
		t.Fatalf("read %q", b)
	}
	got.st.Close()
	<-done
}

func TestClaimRules(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	if _, err := r.Claim("node-a", strings.Repeat("0", 32), strings.NewReader("")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("unknown ID: %v", err)
	}
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	// A request that was not polled cannot be claimed, even with its ID.
	var id string
	for id == "" {
		r.mu.Lock()
		for k := range r.reqs {
			id = k
		}
		r.mu.Unlock()
	}
	if _, err := r.Claim("node-a", id, strings.NewReader("")); !errors.Is(err, ErrUnknown) {
		t.Fatalf("claim before poll: %v", err)
	}
	mustNext(t, r, "node-a")
	done := serveClaim(t, r, "node-a", id, strings.NewReader(""), func() {})
	if _, err := r.Claim("node-a", id, strings.NewReader("")); !errors.Is(err, ErrClaimed) {
		t.Fatalf("second claim: %v", err)
	}
	got := <-res
	got.st.Close()
	<-done
}

func TestStreamCapPerNode(t *testing.T) {
	checkNoLeak(t)
	r := New(quick()) // two streams per node
	defer r.Close()
	a := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	b := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web"})
	time.Sleep(30 * time.Millisecond)
	if _, err := r.Open(context.Background(), "node-a", nodeapi.LogRequest{App: "web"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("third stream: %v", err)
	}
	// Another node has its own cap.
	c := openAsync(r, context.Background(), "node-b", nodeapi.LogRequest{App: "web"})
	// A claimed stream holds its slot until the node side returns.
	q := mustNext(t, r, "node-a")
	done := serveClaim(t, r, "node-a", q.ID, strings.NewReader(""), func() {})
	var st *Stream
	for _, ch := range []chan openResult{a, b} {
		if res := <-ch; res.st != nil {
			st = res.st
		}
	}
	if st == nil {
		t.Fatal("no stream was claimed")
	}
	// One slot is held by the open stream and one is free again, so a new
	// request is queued, and times out unclaimed.
	if _, err := r.Open(context.Background(), "node-a", nodeapi.LogRequest{App: "web"}); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("stream beside the open one: %v", err)
	}
	st.Close()
	<-done
	if res := <-c; !errors.Is(res.err, ErrNoAnswer) {
		t.Fatalf("node-b: %v", res.err)
	}
	if r.nodes["node-a"].active != 0 {
		t.Fatalf("active %d after every stream ended", r.nodes["node-a"].active)
	}
}

func TestPollCapPerNode(t *testing.T) {
	checkNoLeak(t)
	o := quick()
	o.PollTimeout = time.Second
	r := New(o) // one poll per node
	defer r.Close()
	first := make(chan error, 1)
	go func() {
		_, err := r.Next(context.Background(), "node-a")
		first <- err
	}()
	time.Sleep(30 * time.Millisecond)
	if _, err := r.Next(context.Background(), "node-a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second poll: %v", err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestPollTimesOutEmpty(t *testing.T) {
	checkNoLeak(t)
	r := New(quick())
	defer r.Close()
	start := time.Now()
	q, err := r.Next(context.Background(), "node-a")
	if q != nil || err != nil {
		t.Fatalf("got %+v, %v", q, err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 2*time.Second {
		t.Fatalf("poll returned after %s", d)
	}
}

func TestCloseEndsPollsWaitsAndStreams(t *testing.T) {
	checkNoLeak(t)
	o := quick()
	o.PollTimeout, o.PickupTimeout, o.MaxPollsPerNode = 10*time.Second, 10*time.Second, 2
	r := New(o)
	poll := make(chan error, 1)
	go func() {
		_, err := r.Next(context.Background(), "node-b")
		poll <- err
	}()
	waiting := openAsync(r, context.Background(), "node-c", nodeapi.LogRequest{App: "web"})
	res := openAsync(r, context.Background(), "node-a", nodeapi.LogRequest{App: "web", Follow: true})
	q := mustNext(t, r, "node-a")
	pr, pw := io.Pipe()
	done := serveClaim(t, r, "node-a", q.ID, pr, func() { pw.CloseWithError(errors.New("aborted")) })
	got := <-res
	readErr := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(got.st)
		readErr <- err
	}()
	time.Sleep(30 * time.Millisecond)
	r.Close()
	if err := <-poll; !errors.Is(err, ErrClosed) {
		t.Fatalf("poll: %v", err)
	}
	if w := <-waiting; !errors.Is(w.err, ErrClosed) {
		t.Fatalf("waiting open: %v", w.err)
	}
	if err := <-readErr; err == nil {
		t.Fatal("stream read ended without an error")
	}
	got.st.Close()
	<-done
	if _, err := r.Open(context.Background(), "node-a", nodeapi.LogRequest{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("open after close: %v", err)
	}
	if _, err := r.Next(context.Background(), "node-a"); !errors.Is(err, ErrClosed) {
		t.Fatalf("poll after close: %v", err)
	}
	r.Close() // idempotent
}
