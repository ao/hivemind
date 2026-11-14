package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/store"
)

// logRuntime is a runtime with one container per replica of app "web". Its
// log is the lines in lines, alternating stdout and stderr. With Follow, it
// then writes each string sent on more, until ctx ends, and reports the end on
// ended.
type logRuntime struct {
	runtime.Runtime // nil: only List and Logs are called
	lines           []string
	more            chan string
	ended           chan error

	mu       sync.Mutex
	lastOpts runtime.LogOptions
	lastID   string
}

func newLogRuntime(lines ...string) *logRuntime {
	return &logRuntime{lines: lines, more: make(chan string), ended: make(chan error, 16)}
}

func (f *logRuntime) List(_ context.Context, labels map[string]string) ([]runtime.Container, error) {
	if labels[runtime.LabelApp] != "web" {
		return nil, nil
	}
	rep := labels[runtime.LabelReplica]
	return []runtime.Container{
		{ID: "old-" + rep, Name: "hivemind-web-" + rep + "-g1", State: "exited",
			Labels: map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: rep, runtime.LabelGeneration: "1"}},
		{ID: "c-" + rep, Name: "hivemind-web-" + rep + "-g2", State: "running",
			Labels: map[string]string{runtime.LabelApp: "web", runtime.LabelReplica: rep, runtime.LabelGeneration: "2"}},
	}, nil
}

func (f *logRuntime) Logs(ctx context.Context, id string, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	f.mu.Lock()
	f.lastOpts, f.lastID = opts, id
	f.mu.Unlock()
	err := f.logs(ctx, opts, stdout, stderr)
	f.ended <- err
	return err
}

func (f *logRuntime) logs(ctx context.Context, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	for i, l := range f.lines {
		w := stdout
		if i%2 == 1 {
			w = stderr
		}
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return err
		}
	}
	if !opts.Follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case l, ok := <-f.more:
			if !ok {
				return nil // the container exited
			}
			if _, err := io.WriteString(stdout, l+"\n"); err != nil {
				return err
			}
		}
	}
}

// checkNoLeak fails the test if more goroutines run after its cleanups than
// before it started. It retries, because goroutines take a moment to exit.
func checkNoLeak(t *testing.T) {
	t.Helper()
	before := goruntime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			n := goruntime.NumGoroutine()
			if n <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:goruntime.Stack(buf, true)]
				t.Errorf("%d goroutines before, %d after:\n%s", before, n, buf)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// newLogFixture is newFixture with a relay whose timeouts are short.
func newLogFixture(t *testing.T) (*fixture, *logrelay.Relay) {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	relay := logrelay.New(logrelay.Options{PickupTimeout: 2 * time.Second, PollTimeout: time.Second})
	tokens := pki.NewTokens(st.JoinTokens())
	c, err := Listen(Config{Store: st, CA: ca, Tokens: tokens, Advertise: "127.0.0.1", Port: 0,
		LocalNodeID: "node-server", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Logs: relay})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = c.Serve()
	}()
	t.Cleanup(func() {
		_ = c.Shutdown()
		<-served
		st.Close()
	})
	return &fixture{t: t, st: st, ca: ca, tokens: tokens, nodeAddr: c.NodeAddr(), joinAddr: c.JoinAddr()}, relay
}

func (fx *fixture) workerClient(cert tls.Certificate) *nodeapi.Client {
	fx.t.Helper()
	cfg, err := pki.ClientTLSConfig(fx.ca, cert, "127.0.0.1")
	if err != nil {
		fx.t.Fatal(err)
	}
	c, err := nodeapi.NewClient("https://"+fx.nodeAddr, cfg)
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.t.Cleanup(c.CloseIdleConnections)
	return c
}

// startLogServer runs a worker's log server until the test ends.
func startLogServer(t *testing.T, c *nodeapi.Client, id string, rt runtime.Runtime) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ls := &nodeapi.LogServer{Client: c, NodeID: id, Runtime: rt, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		_ = ls.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

type frame struct {
	t    byte
	body string
}

// readFrames reads n frames, or all of them when n is -1.
func readFrames(t *testing.T, fr *nodeapi.FrameReader, n int) []frame {
	t.Helper()
	var out []frame
	for n < 0 || len(out) < n {
		typ, _, err := fr.Next()
		if errors.Is(err, io.EOF) && n < 0 {
			return out
		}
		if err != nil {
			t.Fatalf("after %d frames: %v", len(out), err)
		}
		var b strings.Builder
		if err := fr.Copy(&b); err != nil {
			t.Fatal(err)
		}
		out = append(out, frame{typ, b.String()})
	}
	return out
}

func TestLogRelayReadsAWorkersLogOverMTLS(t *testing.T) {
	checkNoLeak(t)
	fx, relay := newLogFixture(t)
	id, cert := fx.enroll(fx.issue(time.Hour))
	rt := newLogRuntime("out one", "err one", "out two")
	c := fx.workerClient(cert)
	startLogServer(t, c, id, rt)

	// Several streams in a row, each followed by the worker's other calls on
	// the same client. A stream's connection must not be reused for them: the
	// server leaves it unusable when it ends the stream.
	for i := range 3 {
		st, err := relay.Open(context.Background(), id, nodeapi.LogRequest{App: "web", Replica: 1, Tail: 3})
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		got := readFrames(t, nodeapi.NewFrameReader(st), -1)
		st.Close()
		want := []frame{{nodeapi.FrameReady, ""}, {nodeapi.FrameStdout, "out one\n"}, {nodeapi.FrameStderr, "err one\n"},
			{nodeapi.FrameStdout, "out two\n"}}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("stream %d: frames %v, want %v", i, got, want)
		}
		time.Sleep(100 * time.Millisecond) // let the stream's connection go idle
		for j := range 3 {
			if err := c.Register(context.Background(), id, nodeapi.Register{Name: "w", CPU: 1, Memory: 1 << 30}); err != nil {
				t.Fatalf("register %d after stream %d: %v", j, i, err)
			}
			if _, err := c.AssignmentsForNode(context.Background(), id); err != nil {
				t.Fatalf("assignments %d after stream %d: %v", j, i, err)
			}
		}
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.lastID != "c-1" || rt.lastOpts.Tail != 3 || rt.lastOpts.Follow {
		t.Fatalf("worker read %q with %+v", rt.lastID, rt.lastOpts)
	}
}

func TestLogRelayReportsAMissingContainer(t *testing.T) {
	checkNoLeak(t)
	fx, relay := newLogFixture(t)
	id, cert := fx.enroll(fx.issue(time.Hour))
	startLogServer(t, fx.workerClient(cert), id, newLogRuntime())
	st, err := relay.Open(context.Background(), id, nodeapi.LogRequest{App: "api"})
	if err != nil {
		t.Fatal(err)
	}
	got := readFrames(t, nodeapi.NewFrameReader(st), -1)
	st.Close()
	if len(got) != 1 || got[0].t != nodeapi.FrameNotFound || !strings.Contains(got[0].body, "no container") {
		t.Fatalf("frames %v", got)
	}
}

// TestLogRelayFollowEndsOnTheWorkerWhenTheUserLeaves: a followed stream sees
// new lines, and when the API side cancels, the worker's log read is cancelled
// too, and nothing is left running on either side.
func TestLogRelayFollowEndsOnTheWorkerWhenTheUserLeaves(t *testing.T) {
	checkNoLeak(t)
	fx, relay := newLogFixture(t)
	id, cert := fx.enroll(fx.issue(time.Hour))
	rt := newLogRuntime("first")
	startLogServer(t, fx.workerClient(cert), id, rt)

	st, err := relay.Open(context.Background(), id, nodeapi.LogRequest{App: "web", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	fr := nodeapi.NewFrameReader(st)
	if got := readFrames(t, fr, 2); got[1].body != "first\n" {
		t.Fatalf("frames %v", got)
	}
	rt.more <- "second"
	if got := readFrames(t, fr, 1); got[0].body != "second\n" {
		t.Fatalf("followed frame %v", got)
	}
	// The user goes away while the worker has nothing to send.
	readDone := make(chan error, 1)
	go func() {
		_, _, err := fr.Next()
		readDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	st.Cancel()
	if err := <-readDone; err == nil {
		t.Fatal("the read went on after cancel")
	}
	st.Close()
	select {
	case err := <-rt.ended:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("worker log read ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker's log read was not cancelled")
	}
}

// TestLogRelayFollowEndsWhenTheContainerExits: the worker's log ends, and the
// API side reads the end of the stream.
func TestLogRelayFollowEndsWhenTheContainerExits(t *testing.T) {
	checkNoLeak(t)
	fx, relay := newLogFixture(t)
	id, cert := fx.enroll(fx.issue(time.Hour))
	rt := newLogRuntime("only")
	startLogServer(t, fx.workerClient(cert), id, rt)
	st, err := relay.Open(context.Background(), id, nodeapi.LogRequest{App: "web", Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fr := nodeapi.NewFrameReader(st)
	readFrames(t, fr, 2)
	close(rt.more)
	if got := readFrames(t, fr, -1); len(got) != 0 {
		t.Fatalf("frames after exit: %v", got)
	}
}

func TestLogRoutesCheckTheNode(t *testing.T) {
	checkNoLeak(t)
	fx, relay := newLogFixture(t)
	a, certA := fx.enroll(fx.issue(time.Hour))
	b, certB := fx.enroll(fx.issue(time.Hour))
	ca, cb := fx.nodeClient(certA), fx.nodeClient(certB)
	t.Cleanup(ca.CloseIdleConnections)
	t.Cleanup(cb.CloseIdleConnections)

	// B cannot poll A's requests by naming A in the path.
	if code, _ := fx.send(cb, http.MethodGet, fx.nodeURL(nodeapi.LogRequestsPath(a)), nil); code != http.StatusForbidden {
		t.Fatalf("B polling A's path: %d", code)
	}

	type opened struct {
		st  *logrelay.Stream
		err error
	}
	res := make(chan opened, 1)
	go func() {
		st, err := relay.Open(context.Background(), a, nodeapi.LogRequest{App: "web"})
		res <- opened{st, err}
	}()
	// B's own poll does not see A's request.
	if code, body := fx.send(cb, http.MethodGet, fx.nodeURL(nodeapi.LogRequestsPath(b)), nil); code != http.StatusNoContent {
		t.Fatalf("B's poll: %d %s", code, body)
	}
	code, body := fx.send(ca, http.MethodGet, fx.nodeURL(nodeapi.LogRequestsPath(a)), nil)
	if code != http.StatusOK {
		t.Fatalf("A's poll: %d %s", code, body)
	}
	var q nodeapi.LogRequest
	if err := json.Unmarshal(body, &q); err != nil || q.Validate() != nil {
		t.Fatalf("poll reply %s: %v", body, err)
	}

	// B cannot stream for A's request: not on its own path, where the relay
	// refuses it, nor on A's path, where the certificate check does.
	if code, _ := fx.send(cb, http.MethodPost, fx.nodeURL(nodeapi.LogStreamPath(b, q.ID)), []byte("x")); code != http.StatusForbidden {
		t.Fatalf("B streaming A's request on its own path: %d", code)
	}
	if code, _ := fx.send(cb, http.MethodPost, fx.nodeURL(nodeapi.LogStreamPath(a, q.ID)), []byte("x")); code != http.StatusForbidden {
		t.Fatalf("B streaming on A's path: %d", code)
	}
	// Malformed and unknown IDs.
	if code, _ := fx.send(ca, http.MethodPost, fx.nodeURL(nodeapi.LogStreamPath(a, "nope")), nil); code != http.StatusBadRequest {
		t.Fatalf("malformed ID: %d", code)
	}
	if code, _ := fx.send(ca, http.MethodPost, fx.nodeURL(nodeapi.LogStreamPath(a, strings.Repeat("0", 32))), nil); code != http.StatusNotFound {
		t.Fatalf("unknown ID: %d", code)
	}

	// The request is intact, and A's stream gets through.
	var frames strings.Builder
	fw := nodeapi.NewFrameWriter(&frames)
	_ = fw.WriteFrame(nodeapi.FrameReady, nil)
	_ = fw.WriteFrame(nodeapi.FrameStdout, []byte("from A\n"))
	streamed := make(chan int, 1)
	go func() {
		code, _ := fx.send(ca, http.MethodPost, fx.nodeURL(nodeapi.LogStreamPath(a, q.ID)), []byte(frames.String()))
		streamed <- code
	}()
	o := <-res
	if o.err != nil {
		t.Fatal(o.err)
	}
	got := readFrames(t, nodeapi.NewFrameReader(o.st), -1)
	o.st.Close()
	if len(got) != 2 || got[1].body != "from A\n" {
		t.Fatalf("frames %v", got)
	}
	if code := <-streamed; code != http.StatusNoContent {
		t.Fatalf("A's stream: %d", code)
	}
}

func TestRemovedNodeCannotPollLogs(t *testing.T) {
	fx, _ := newLogFixture(t)
	id, cert := fx.enroll(fx.issue(time.Hour))
	c := fx.nodeClient(cert)
	t.Cleanup(c.CloseIdleConnections)
	fx.registerNode(c, id)
	if _, err := fx.st.RemoveNode(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if code, body := fx.send(c, http.MethodGet, fx.nodeURL(nodeapi.LogRequestsPath(id)), nil); code != http.StatusForbidden ||
		!strings.Contains(string(body), nodeapi.RemovedMessage) {
		t.Fatalf("removed node's poll: %d %s", code, body)
	}
	wc := fx.workerClient(cert)
	if _, err := wc.NextLogRequest(context.Background(), id); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Fatalf("client poll: %v", err)
	}
}
