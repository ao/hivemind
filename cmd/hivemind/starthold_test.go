package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/store"
)

// When the start grace ends with no report from any worker that was
// waiting, the sweep keeps them waiting and serve says so at error, with
// what to do, once and then once an hour. When one of them reports, it
// says, once, that the hold is over and until when the others wait, and
// they are lost only then.
func TestStartHoldIsLogged(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hivemind.db"), store.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, id := range []string{"node-1", "node-2"} {
		if err := st.UpsertNode(ctx, store.Node{ID: id, Name: id, CPU: 1, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	advance(10 * time.Minute)
	started := clock()
	if err := st.SetStarted(ctx, started, time.Minute); err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	var hold startHoldLog
	sweepAfter := func(d time.Duration) []string {
		t.Helper()
		advance(d)
		lost, err := st.ReconcileNodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hold.note(ctx, st, log, started)
		return lost
	}
	errorLines := func() int { return strings.Count(logs.String(), "level=ERROR") }

	if lost := sweepAfter(30 * time.Second); len(lost) != 0 || errorLines() != 0 {
		t.Fatalf("in the grace: lost %v, logs:\n%s", lost, logs)
	}
	if lost := sweepAfter(31 * time.Second); len(lost) != 0 || errorLines() != 1 {
		t.Fatalf("past the grace, no worker having reported: lost %v, want none, and one error:\n%s", lost, logs)
	}
	l := logs.String()
	for _, want := range []string{"no worker has reported to this server since it started, and its start grace has ended",
		"stop this server and disable its service", "hivemind node remove <id>", "docs/backup.md#if-the-old-server-comes-back",
		"waiting=2", "nodes=node-1,node-2"} {
		if !strings.Contains(l, want) {
			t.Errorf("the line does not say %q:\n%s", want, l)
		}
	}
	for range 3 {
		sweepAfter(5 * time.Minute)
	}
	if errorLines() != 1 {
		t.Fatalf("logged %d times within the hour, want once:\n%s", errorLines(), logs)
	}
	sweepAfter(time.Hour)
	if errorLines() != 2 {
		t.Fatalf("logged %d times after an hour, want twice:\n%s", errorLines(), logs)
	}

	// node-1 reports: node-2 waits for the start grace again, a minute
	// here, from that report.
	reported := clock()
	if err := st.RenewLease(ctx, "node-1"); err != nil {
		t.Fatal(err)
	}
	if lost := sweepAfter(time.Second); len(lost) != 0 {
		t.Fatalf("1 s after node-1's report: lost %v, want none", lost)
	}
	l = logs.String()
	for _, want := range []string{"which ends the start grace's hold", "wait for the start grace again", "waiting=1", "nodes=node-2",
		"hold_ended=" + reported.UTC().Format(time.RFC3339), "wait_ends=" + reported.Add(time.Minute).UTC().Format(time.RFC3339)} {
		if !strings.Contains(l, want) {
			t.Errorf("the end of the hold does not say %q:\n%s", want, l)
		}
	}
	// node-1 renews on, as a live worker does.
	for range 6 {
		advance(9 * time.Second)
		if err := st.RenewLease(ctx, "node-1"); err != nil {
			t.Fatal(err)
		}
	}
	if lost := sweepAfter(4 * time.Second); len(lost) != 0 {
		t.Fatalf("59 s after node-1's report: lost %v, want none", lost)
	}
	if lost := sweepAfter(time.Second); !slices.Equal(lost, []string{"node-2"}) {
		t.Fatalf("a minute after node-1's report: lost %v, want node-2", lost)
	}
	sweepAfter(2 * time.Hour)
	if errorLines() != 2 || strings.Count(logs.String(), "ends the start grace's hold") != 1 {
		t.Fatalf("after the hold:\n%s", logs)
	}
}

// When every worker that was waiting has reported by the sweep after the
// hold ends, the line says so, and names no wait.
func TestStartHoldEndsWithEveryWorkerBack(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hivemind.db"), store.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "node-1", CPU: 1, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(10 * time.Minute)
	mu.Unlock()
	started := clock()
	if err := st.SetStarted(ctx, started, time.Minute); err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	var hold startHoldLog
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	hold.note(ctx, st, log, started)
	if err := st.RenewLease(ctx, "node-1"); err != nil {
		t.Fatal(err)
	}
	hold.note(ctx, st, log, started)
	hold.note(ctx, st, log, started)
	l := logs.String()
	if strings.Count(l, "level=ERROR") != 1 || strings.Count(l, "every worker that was waiting has reported") != 1 || strings.Contains(l, "wait_ends") {
		t.Fatalf("logs:\n%s", l)
	}
}
