package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// A crash-looping replica's status says why its container stopped: the exit
// code, or the kernel's OOM kill. (The failed health check is in the edge
// suite's crashloop scenario, as its probes run on the real clock.)
func TestCrashLoopStatusSaysWhy(t *testing.T) {
	e := newEnv(t)
	// The node has room for the memory the replica reserves.
	if err := e.st.UpsertNode(ctxT(t), store.Node{ID: nodeID, Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	e.put(spec.App{Name: "web", Image: "busybox", Replicas: 1, Resources: spec.Resources{Memory: 8 << 20}})
	e.reconcile()
	id := e.running()["hivemind-web-0-g1"].ID
	e.rt.exit(id, 3)
	e.clock.Advance(time.Second)
	e.reconcile() // the first restart is due at once
	e.rt.exit(id, 3)
	e.reconcile() // the second waits 2 s
	st := e.status("web")[0]
	if !st.CrashLooping || st.Error != "exited with code 3" {
		t.Fatalf("status after an exit: crash looping %v, error %q", st.CrashLooping, st.Error)
	}

	e.clock.Advance(2 * time.Second)
	e.reconcile()
	e.rt.mu.Lock()
	c := e.rt.find(id)
	c.State, c.ExitCode, c.OOMKilled, c.FinishedAt = "exited", 137, true, e.clock.Now()
	e.rt.mu.Unlock()
	e.reconcile()
	if st := e.status("web")[0]; !strings.Contains(st.Error, "memory limit of 8Mi (out of memory)") {
		t.Fatalf("status after an OOM kill: %q", st.Error)
	}

	// Once it runs again, the reason is gone.
	e.clock.Advance(time.Minute)
	e.reconcile()
	if st := e.status("web")[0]; st.State != "running" || st.Error != "" || st.CrashLooping {
		t.Fatalf("status after the restart: %+v", st)
	}
}
