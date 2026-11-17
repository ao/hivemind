package nodeapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseEpoch(t *testing.T) {
	for v, want := range map[string]int64{"0": 0, "1": 1, "42": 42, "9223372036854775807": 1<<63 - 1} {
		if got, ok := ParseEpoch(v); !ok || got != want {
			t.Errorf("ParseEpoch(%q) = %d, %v; want %d", v, got, ok, want)
		}
	}
	for _, v := range []string{"", "-1", "+1", " 1", "1 ", "1.0", "0x1", "abc", "9223372036854775808", "99999999999999999999"} {
		if got, ok := ParseEpoch(v); ok {
			t.Errorf("ParseEpoch(%q) = %d, true; want not an epoch", v, got)
		}
	}
	if FormatEpoch(7) != "7" {
		t.Errorf("FormatEpoch(7) = %q", FormatEpoch(7))
	}
}

// epochServer is a node API server whose replies carry the epoch in its
// field ("" for none), and that counts the requests it gets.
type epochServer struct {
	ts       *httptest.Server
	epoch    atomic.Pointer[string]
	requests atomic.Int64
	mu       sync.Mutex
	sent     []string // the EpochHeader of each request
}

func newEpochServer(t *testing.T, epoch string) *epochServer {
	s := &epochServer{}
	s.set(epoch)
	s.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.mu.Lock()
		s.sent = append(s.sent, r.Header.Get(EpochHeader))
		s.mu.Unlock()
		if e := *s.epoch.Load(); e != "" {
			w.Header().Set(EpochHeader, e)
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assignments":[]}`))
	}))
	t.Cleanup(s.ts.Close)
	return s
}

func (s *epochServer) set(epoch string) { s.epoch.Store(&epoch) }

func (s *epochServer) lastSent() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return ""
	}
	return s.sent[len(s.sent)-1]
}

// A guarded client sends the highest epoch it has seen, raises and saves it
// on a higher reply, and refuses a lower one: it logs that once, sends that
// server nothing for Retry, then asks it once again. A reply without an
// epoch, from a server older than epochs, is taken, and so is the server at
// the right epoch once it is back.
func TestEpochGuard(t *testing.T) {
	srv := newEpochServer(t, "1")
	var now atomic.Pointer[time.Time]
	start := time.Unix(1_700_000_000, 0)
	now.Store(&start)
	advance := func(d time.Duration) { n := now.Load().Add(d); now.Store(&n) }
	logs := &syncBuf{}
	var saves []int64
	g := NewEpochGuard(1)
	g.Save = func(e int64) error { saves = append(saves, e); return nil }
	g.Log = slog.New(slog.NewTextHandler(logs, nil))
	g.Retry = 10 * time.Second
	g.Now = func() time.Time { return *now.Load() }
	c, err := NewClient(srv.ts.URL, srv.ts.Client().Transport.(*http.Transport).TLSClientConfig, WithEpochGuard(g))
	if err != nil {
		t.Fatal(err)
	}
	var answered atomic.Int64
	c.OnAnswer(func() { answered.Add(1) })
	ctx := context.Background()
	get := func() error { _, err := c.AssignmentsForNode(ctx, "node-abc123"); return err }
	errorLines := func() int { return strings.Count(logs.String(), "level=ERROR") }

	if err := get(); err != nil || srv.lastSent() != "1" || len(saves) != 0 {
		t.Fatalf("at the epoch seen: %v, sent %q, saves %v", err, srv.lastSent(), saves)
	}
	// A restored server, at a higher epoch.
	srv.set("3")
	if err := get(); err != nil || g.Highest() != 3 || len(saves) != 1 || saves[0] != 3 {
		t.Fatalf("at a higher epoch: %v, highest %d, saves %v", err, g.Highest(), saves)
	}
	if err := c.ReportStatus(ctx, "node-abc123", nil); err != nil || srv.lastSent() != "3" {
		t.Fatalf("after the raise: %v, sent %q", err, srv.lastSent())
	}

	// The server from before the restore answers at that address.
	srv.set("2")
	before := answered.Load()
	err = get()
	var stale *StaleServerError
	var u interface{ Unreachable() bool }
	if !errors.As(err, &stale) || stale.Epoch != 2 || stale.Seen != 3 || !errors.As(err, &u) || !u.Unreachable() {
		t.Fatalf("a server at a lower epoch: %v", err)
	}
	if answered.Load() != before {
		t.Error("a reply from a stale server counted as the server answering")
	}
	if errorLines() != 1 || !strings.Contains(logs.String(), "is from before a restore of this cluster") ||
		!strings.Contains(logs.String(), "hivemind node set-server <address>") || !strings.Contains(logs.String(), "server_epoch=2 seen_epoch=3") {
		t.Fatalf("the refusal's log:\n%s", logs.String())
	}
	// Held off: nothing is sent, a request with a body included, until
	// Retry has passed.
	n := srv.requests.Load()
	for range 3 {
		if err := get(); !errors.As(err, &stale) {
			t.Fatalf("held off: %v", err)
		}
	}
	if err := c.ReportStatus(ctx, "node-abc123", nil); !errors.As(err, &stale) {
		t.Fatalf("a report while held off: %v", err)
	}
	advance(9 * time.Second)
	if err := get(); !errors.As(err, &stale) {
		t.Fatalf("held off at 9 s: %v", err)
	}
	if got := srv.requests.Load(); got != n {
		t.Fatalf("%d requests sent while held off", got-n)
	}
	// After Retry, one request asks again; it is still stale.
	advance(time.Second)
	if err := get(); !errors.As(err, &stale) {
		t.Fatalf("asked again: %v", err)
	}
	if got := srv.requests.Load(); got != n+1 {
		t.Fatalf("%d requests sent when asking again, want 1", got-n)
	}
	if err := get(); !errors.As(err, &stale) || srv.requests.Load() != n+1 {
		t.Fatalf("just after asking again: %v, %d requests", err, srv.requests.Load()-n)
	}
	if errorLines() != 1 {
		t.Fatalf("the refusal was logged %d times, want once:\n%s", errorLines(), logs.String())
	}

	// A server older than epochs at the address is taken, as before.
	srv.set("")
	advance(10 * time.Second)
	if err := get(); err != nil || g.Highest() != 3 {
		t.Fatalf("a server without an epoch: %v, highest %d", err, g.Highest())
	}
	if !strings.Contains(logs.String(), "answers without an epoch now") {
		t.Fatalf("taking a server without an epoch was not logged:\n%s", logs.String())
	}
	// The stale server again: a new refusal, logged again.
	srv.set("0")
	if err := get(); !errors.As(err, &stale) || stale.Epoch != 0 || errorLines() != 2 {
		t.Fatalf("the stale server again: %v, %d error lines", err, errorLines())
	}
	// The restored server comes back at the address.
	srv.set("3")
	advance(10 * time.Second)
	if err := get(); err != nil {
		t.Fatalf("the restored server back: %v", err)
	}
	if !strings.Contains(logs.String(), "answers at this cluster's epoch now; this worker talks to it again") {
		t.Fatalf("taking the restored server again was not logged:\n%s", logs.String())
	}
	if err := get(); err != nil || len(saves) != 1 {
		t.Fatalf("after: %v, saves %v", err, saves)
	}
}

// A save that fails is logged once, and tried again on a later reply, no
// more than once a minute, until it works.
func TestEpochGuardRetriesAFailedSave(t *testing.T) {
	srv := newEpochServer(t, "5")
	now := time.Unix(1_700_000_000, 0)
	logs := &syncBuf{}
	tries, fail := 0, true
	g := NewEpochGuard(2)
	g.Save = func(int64) error {
		tries++
		if fail {
			return errors.New("no space left on device")
		}
		return nil
	}
	g.Log = slog.New(slog.NewTextHandler(logs, nil))
	g.Now = func() time.Time { return now }
	c, err := NewClient(srv.ts.URL, srv.ts.Client().Transport.(*http.Transport).TLSClientConfig, WithEpochGuard(g))
	if err != nil {
		t.Fatal(err)
	}
	get := func() {
		t.Helper()
		if _, err := c.AssignmentsForNode(context.Background(), "node-abc123"); err != nil {
			t.Fatal(err)
		}
	}
	get()
	get()
	if tries != 1 || g.Highest() != 5 || strings.Count(logs.String(), "level=WARN") != 1 {
		t.Fatalf("%d tries, highest %d, logs:\n%s", tries, g.Highest(), logs.String())
	}
	now = now.Add(time.Minute)
	get()
	if tries != 2 || strings.Count(logs.String(), "level=WARN") != 1 {
		t.Fatalf("a minute later: %d tries, logs:\n%s", tries, logs.String())
	}
	fail = false
	now = now.Add(time.Minute)
	get()
	get()
	if tries != 3 {
		t.Fatalf("after a good save: %d tries, want 3", tries)
	}
}

// Without a guard the client sends no epoch and takes any, as before.
func TestNoEpochGuard(t *testing.T) {
	srv := newEpochServer(t, "0")
	c, err := NewClient(srv.ts.URL, srv.ts.Client().Transport.(*http.Transport).TLSClientConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AssignmentsForNode(context.Background(), "node-abc123"); err != nil || srv.lastSent() != "" {
		t.Fatalf("%v, sent %q", err, srv.lastSent())
	}
}
