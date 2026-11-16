package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// unreachableErr is what the node API client returns when the server does
// not answer (nodeapi.UnreachableError).
type unreachableErr struct{}

func (unreachableErr) Error() string {
	return "calling the server: dial tcp 10.0.0.5:4484: connect: connection refused"
}

func (unreachableErr) Unreachable() bool { return true }

func TestOutageWait(t *testing.T) {
	const s = time.Second
	for failures, want := range map[int]time.Duration{1: 2 * s, 2: 4 * s, 3: 8 * s, 4: 16 * s, 5: 30 * s, 6: 30 * s, 100: 30 * s} {
		if got := (outage{failures: failures}).wait(DefaultInterval, DefaultUnreachableWait); got != want {
			t.Errorf("wait after %d failed passes: %s, want %s", failures, got, want)
		}
	}
	// An interval longer than the most is kept.
	if got := (outage{failures: 3}).wait(time.Hour, DefaultUnreachableWait); got != time.Hour {
		t.Errorf("wait with a one-hour interval: %s", got)
	}
}

// While the server cannot be reached, the passes back off from the interval,
// doubling, to UnreachableWait; the outage is logged when it starts, then
// every OutageLogEvery with a count, and once when the server answers again,
// with no line per pass. Then the passes are back at the interval, and the
// first comes within one backed-off wait.
func TestRunBacksOffWhileTheServerIsUnreachable(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	e.reconcile()
	const (
		interval = 10 * time.Millisecond
		most     = 80 * time.Millisecond
		logEvery = 150 * time.Millisecond
	)
	var mu sync.Mutex
	var passes []time.Time
	var failed []bool
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, Interval: interval,
		UnreachableWait: most, OutageLogEvery: logEvery, Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
		OnPass: func(_ time.Duration, err error) {
			mu.Lock()
			passes = append(passes, time.Now())
			failed = append(failed, err != nil)
			mu.Unlock()
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.src.setFail(unreachableErr{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	time.Sleep(time.Second)
	mu.Lock()
	down := append([]time.Time(nil), passes...)
	mu.Unlock()
	// 10, 20, 40 and then 80 ms between passes: about 15 in a second, where
	// passes at the interval would be 100.
	if n := len(down); n < 6 || n > 25 {
		t.Fatalf("%d passes in 1 s with the server down, want about 15", n)
	}
	for i := 5; i < len(down); i++ {
		if gap := down[i].Sub(down[i-1]); gap < most-15*time.Millisecond || gap > most+250*time.Millisecond {
			t.Errorf("gap %d between passes with the server down: %s, want about %s", i, gap, most)
		}
	}
	logs := e.logs.String()
	if n := strings.Count(logs, "cannot reach the server; passes back off, up to one every 80ms, until it answers"); n != 1 {
		t.Errorf("%d lines for the start of the outage, want 1:\n%s", n, logs)
	}
	if n := strings.Count(logs, "the server is still unreachable"); n < 3 || n > 7 {
		t.Errorf("%d lines for the outage going on in 1 s, every 150 ms, want about 6:\n%s", n, logs)
	}
	if !strings.Contains(logs, "failed_passes=") || strings.Contains(logs, "reconcile pass failed") {
		t.Errorf("the outage lines have no count, or a pass logged its own failure:\n%s", logs)
	}

	back := time.Now()
	e.src.setFail(nil)
	var first time.Time
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for i := len(passes) - 1; i >= 0 && passes[i].After(back); i-- {
			if !failed[i] {
				first = passes[i]
			}
		}
		return !first.IsZero()
	})
	if d := first.Sub(back); d > most+250*time.Millisecond {
		t.Errorf("the first pass after the server came back was %s later, more than one backed-off wait (%s)", d, most)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := 0
	for _, p := range passes {
		if p.After(first) {
			n++
		}
	}
	mu.Unlock()
	if n < 8 {
		t.Errorf("%d passes in the 200 ms after the server came back, want about 20 at the 10 ms interval", n)
	}
	logs = e.logs.String()
	if c := strings.Count(logs, "the server answers again"); c != 1 {
		t.Errorf("%d lines for the end of the outage, want 1:\n%s", c, logs)
	}
}

// A pass that fails for another reason, such as a refusal from a server
// that answers, is logged each time, as before, and does not back off.
func TestRunDoesNotBackOffForOtherErrors(t *testing.T) {
	e := newEnv(t)
	e.src.setFail(errors.New("the server refused the request (500 Internal Server Error): the store is read-only"))
	var mu sync.Mutex
	passes := 0
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Clock: e.clock.Now, Interval: 10 * time.Millisecond,
		UnreachableWait: time.Hour, Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
		OnPass: func(time.Duration, error) { mu.Lock(); passes++; mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if passes < 10 {
		t.Fatalf("%d passes in 300 ms at a 10 ms interval: a refusal backed off", passes)
	}
	if strings.Contains(e.logs.String(), "cannot reach the server") || !strings.Contains(e.logs.String(), "reconcile pass failed") {
		t.Fatalf("a refusal was logged as an outage, or not at all:\n%s", e.logs.String())
	}
}
