//go:build integration

package docker

// This file sorts after the other test files, so Go runs it last. It kills the
// daemon while a call is in flight, and every later test would fail once that
// happens. The script watches for the marker file this test writes, and then
// kills the daemon's container.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// daemonStopMarker is written just before the blocking call starts.
const daemonStopMarker = "/tmp/hivemind-it-daemon-stop"

func TestIntegrationDaemonStoppedMidCall(t *testing.T) {
	c := itClient(t)
	itPull(t, c, alpineImage)
	t.Cleanup(func() { _ = os.Remove(daemonStopMarker) })

	// PID 1 ignores SIGTERM, so Stop blocks for its full timeout. That gives
	// the script time to kill the daemon during the call.
	id := itCreate(t, c, runtime.ContainerConfig{
		Name:    itName("midcall"),
		Image:   alpineImage,
		Command: []string{"sh", "-c", "trap '' TERM; while true; do sleep 1; done"},
		App:     "midcall",
	})
	if err := c.Start(itCtx(t), id); err != nil {
		t.Fatal(err)
	}
	itWaitState(t, c, id, "running", 30*time.Second)

	if err := os.WriteFile(daemonStopMarker, []byte("stop the daemon now\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	began := time.Now()
	err := c.Stop(ctx, id, 120*time.Second)
	elapsed := time.Since(began)

	if !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("stop while the daemon is killed: err = %v, want ErrUnavailable", err)
	}
	if elapsed > 100*time.Second {
		t.Errorf("the call returned after %s, which means it waited out the timeout instead of failing", elapsed)
	}

	// With the daemon gone, every call fails the same way.
	_, err = c.Info(ctxWithTimeout(t, 30*time.Second))
	if !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("info after the daemon stopped: err = %v, want ErrUnavailable", err)
	}
}

func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}
