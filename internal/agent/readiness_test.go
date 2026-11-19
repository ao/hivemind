package agent

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
)

// A node that starts 40 health-checked containers in one pass, on an engine
// that takes 25 ms to start each, reports each one ready within one health
// check interval and one pass interval of its own start. Before, a
// container's monitor started at the next pass and checked one interval
// after that, and its readiness waited for the pass after the check: in the
// second scale run a node that started 93 containers in one 21 s pass had
// its health-checked ones ready 9 s late.
func TestContainersStartedInOnePassAreReadyOnTime(t *testing.T) {
	const (
		n        = 40
		interval = time.Second     // the health check's
		pass     = 2 * time.Second // the agent's
	)
	e := newEnv(t)
	// Real time throughout: the monitors and the passes run on it.
	e.rt.now = time.Now
	e.rt.startDelay = 25 * time.Millisecond
	e.rt.setExec(func(context.Context, string, []string) (int, error) { return 0, nil })
	a, err := New(Config{NodeID: nodeID, Runtime: e.rt, Source: e.src, Interval: pass,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	app := web(n)
	app.Ports = nil
	app.Health = &spec.Health{Exec: []string{"true"}, Interval: spec.Duration(interval), Timeout: spec.Duration(time.Second), Retries: 3}
	e.put(app)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	readyAt := map[int]time.Time{}
	deadline := time.Now().Add(30 * time.Second)
	for len(readyAt) < n && time.Now().Before(deadline) {
		now := time.Now()
		for _, st := range e.status("web") {
			if _, seen := readyAt[st.Replica]; st.Ready && !seen {
				readyAt[st.Replica] = now
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(readyAt) < n {
		t.Fatalf("%d of %d replicas reported ready in 30 s", len(readyAt), n)
	}
	var worst time.Duration
	for _, c := range e.rt.snapshot() {
		i, err := strconv.Atoi(c.Labels[runtime.LabelReplica])
		if err != nil || c.StartedAt.IsZero() {
			t.Fatalf("container %s: replica %q, started %v", c.Name, c.Labels[runtime.LabelReplica], c.StartedAt)
		}
		lag := readyAt[i].Sub(c.StartedAt)
		worst = max(worst, lag)
		if lag > interval+pass {
			t.Errorf("replica %d was reported ready %v after its container started, more than the check interval and a pass (%v)", i, lag, interval+pass)
		}
	}
	t.Logf("the latest of %d replicas was reported ready %v after its start", n, worst)
}
