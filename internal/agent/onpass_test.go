package agent

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// TestOnPassReportsEachPass checks that Run calls OnPass after each pass,
// with the pass's error.
func TestOnPassReportsEachPass(t *testing.T) {
	e := newEnv(t)
	e.put(web(1))
	var mu sync.Mutex
	failures := 1
	e.rt.setFail(func(op, _ string) error {
		mu.Lock()
		defer mu.Unlock()
		if op == "create" && failures > 0 {
			failures--
			return rtErr("create", runtime.ErrUnavailable)
		}
		return nil
	})
	var passes, errs int
	var durations []time.Duration
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Interval: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
		OnPass: func(d time.Duration, err error) {
			mu.Lock()
			defer mu.Unlock()
			passes++
			if err != nil {
				errs++
			}
			durations = append(durations, d)
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return passes >= 3 })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if errs != 1 {
		t.Errorf("%d passes reported an error, want 1", errs)
	}
	for _, d := range durations {
		if d < 0 {
			t.Errorf("negative pass duration %s", d)
		}
	}
}
