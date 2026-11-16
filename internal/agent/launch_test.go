package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
)

// A create that fails the same way every time, as on a full disk, is tried
// again after 2 s, 4 s, 8 s and so on, up to a minute, not at every pass.
// The status keeps the error and says when the next try is.
func TestFailingCreateBacksOff(t *testing.T) {
	e := newEnv(t)
	e.rt.mu.Lock()
	e.rt.images["docker.io/library/busybox:latest"] = true
	e.rt.mu.Unlock()
	full := &runtime.Error{Op: "create", Message: "write /var/lib/docker/containerd/daemon/meta.db: no space left on device"}
	e.rt.setFail(func(op, _ string) error {
		if op == "create" {
			return full
		}
		return nil
	})
	e.put(spec.App{Name: "web", Image: "busybox", Replicas: 1})
	start := e.clock.Now()
	var tries []time.Duration
	for range 300 { // 300 passes, 1 s apart
		e.rt.counts(true)
		_ = e.agent.Reconcile(ctxT(t))
		if e.rt.counts(false)["create"] > 0 {
			tries = append(tries, e.clock.Now().Sub(start))
		}
		e.clock.Advance(time.Second)
	}
	want := []time.Duration{0, 2 * time.Second, 6 * time.Second, 14 * time.Second, 30 * time.Second,
		62 * time.Second, 122 * time.Second, 182 * time.Second, 242 * time.Second}
	if len(tries) != len(want) {
		t.Fatalf("creates at %v in 300 s, want %v", tries, want)
	}
	for i := range want {
		if tries[i] != want[i] {
			t.Fatalf("creates at %v, want %v", tries, want)
		}
	}
	st := e.status("web")
	if len(st) != 1 || st[0].State != "pending" || !strings.Contains(st[0].Error, "no space left on device") ||
		!strings.Contains(st[0].Error, "next try after") {
		t.Fatalf("status during the backoff: %+v", st)
	}
	if w := e.agent.wake(); w.IsZero() {
		t.Fatal("no wake for the next try")
	}

	// Space comes back: the next try runs the replica, and the state is gone.
	e.rt.setFail(nil)
	e.clock.Advance(launchCap)
	e.reconcile()
	if len(e.running()) != 1 || len(e.agent.launches) != 0 {
		t.Fatalf("after the space came back: running %v, launch state %v", names(e.running()), e.agent.launches)
	}
	if st := e.status("web"); st[0].Error != "" {
		t.Fatalf("error left after the start: %q", st[0].Error)
	}
}

// A created container whose start fails backs off the same way.
func TestFailingStartBacksOff(t *testing.T) {
	e := newEnv(t)
	e.put(spec.App{Name: "web", Image: "busybox", Replicas: 1})
	e.rt.setFail(func(op, _ string) error {
		if op == "start" {
			return &runtime.Error{Op: "start", Message: "mkdir /var/lib/docker/overlay2/x: no space left on device"}
		}
		return nil
	})
	_ = e.agent.Reconcile(ctxT(t)) // created, the start fails
	if c := e.rt.snapshot()["hivemind-web-0-g1"]; c.State != "created" {
		t.Fatalf("state %q, want created", c.State)
	}
	e.rt.counts(true)
	for range 5 {
		e.clock.Advance(300 * time.Millisecond)
		_ = e.agent.Reconcile(ctxT(t))
	}
	if n := e.rt.counts(false)["start"]; n != 0 {
		t.Fatalf("%d starts in the 2 s launch backoff", n)
	}
	if st := e.status("web"); len(st) != 1 || st[0].State != "created" || !strings.Contains(st[0].Error, "next try after") {
		t.Fatalf("status: %+v", st)
	}
	e.rt.setFail(nil)
	e.clock.Advance(launchBase)
	e.reconcile()
	if len(e.running()) != 1 {
		t.Fatal("not started once the backoff was over")
	}
}

func TestLaunchBackoff(t *testing.T) {
	want := []time.Duration{0, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, time.Minute, time.Minute}
	for n := 1; n < len(want); n++ {
		if got := launchBackoff(n); got != want[n] {
			t.Errorf("launchBackoff(%d) = %s, want %s", n, got, want[n])
		}
	}
	if got := launchBackoff(1000); got != launchCap {
		t.Errorf("launchBackoff(1000) = %s", got)
	}
}
