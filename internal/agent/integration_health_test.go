//go:build integration

// These tests need a real Docker daemon, and run through
// scripts/integration-docker.sh like the other integration tests. Each test
// deploys one app and waits for the agent to act on it.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// itHealthSetup opens a fresh store with the local node registered, and returns
// an app name. The app's containers are removed when the test ends.
func itHealthSetup(t *testing.T, rt *docker.Client) (*store.Store, string) {
	t.Helper()
	var b [4]byte
	_, _ = rand.Read(b[:])
	app := "hc-" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: app})
		for _, c := range cs {
			_ = rt.Remove(context.Background(), c.ID, true)
		}
	})
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	info, err := rt.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "local", CPU: info.NCPU, Memory: info.MemTotal}); err != nil {
		t.Fatal(err)
	}
	return st, app
}

// itWaitReplica waits until the app's one replica reports a status that
// satisfies cond, and returns that status.
func itWaitReplica(t *testing.T, st *store.Store, app string, timeout time.Duration, what string, cond func(store.Status) bool) store.Status {
	t.Helper()
	var last store.Status
	itWait(t, what, timeout, func() bool {
		ss, _ := st.ListStatus(context.Background(), app)
		if len(ss) != 1 {
			return false
		}
		last = ss[0]
		return cond(last)
	})
	return last
}

// TestIntegrationExitedContainerRestarts: a container that exits is started
// again in place, and its restart count goes up. Only one container exists
// throughout.
func TestIntegrationExitedContainerRestarts(t *testing.T) {
	rt := itRuntime(t)
	st, app := itHealthSetup(t, rt)
	ag, stop := itAgent(t, rt, st)
	defer stop()

	ctx := context.Background()
	sp := spec.App{Name: app, Image: "nginx:alpine", Replicas: 1,
		Command: []string{"sh", "-c", "sleep 2; exit 0"}}
	if _, err := st.PutApp(ctx, sp); err != nil {
		t.Fatal(err)
	}
	ag.Trigger()
	first := itWaitReplica(t, st, app, 60*time.Second, "container running", func(s store.Status) bool {
		return s.State == "running" && s.ContainerID != ""
	})

	s := itWaitReplica(t, st, app, 90*time.Second, "restart count 2", func(s store.Status) bool {
		return s.RestartCount >= 2
	})
	if s.ContainerID != first.ContainerID {
		t.Fatalf("container ID changed across restarts: %s, was %s", s.ContainerID, first.ContainerID)
	}
	all, _ := itRunning(t, rt, app)
	if all != 1 {
		t.Fatalf("%d containers for the replica, want 1", all)
	}
	t.Logf("exit restart: restart count %d, same container %s", s.RestartCount, shortID(s.ContainerID))
}

// TestIntegrationFailingHTTPProbeRestarts: nginx serves 404 for a path it does
// not have, so its probe fails on every check and the agent restarts it.
func TestIntegrationFailingHTTPProbeRestarts(t *testing.T) {
	rt := itRuntime(t)
	st, app := itHealthSetup(t, rt)
	ag, stop := itAgent(t, rt, st)
	defer stop()

	sp := spec.App{Name: app, Image: "nginx:alpine", Replicas: 1, Health: &spec.Health{
		HTTP:     &spec.HTTPCheck{Path: "/no-such-path", Port: 80},
		Interval: spec.Duration(time.Second),
		Timeout:  spec.Duration(time.Second),
		Retries:  2,
	}}
	if _, err := st.PutApp(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	ag.Trigger()
	s := itWaitReplica(t, st, app, 120*time.Second, "restart after the failing probe", func(s store.Status) bool {
		return s.RestartCount >= 1
	})
	if s.Ready {
		t.Fatalf("a replica failing its probe reported ready: %+v", s)
	}
	t.Logf("failing HTTP probe: restart count %d, health %q", s.RestartCount, s.Health)
}

// TestIntegrationHealthyProbeIsReady: an HTTP probe that passes makes the
// replica healthy and ready. An exec probe is checked the same way, which
// exercises the Docker exec API against a real daemon.
func TestIntegrationHealthyProbeIsReady(t *testing.T) {
	rt := itRuntime(t)
	st, app := itHealthSetup(t, rt)
	ag, stop := itAgent(t, rt, st)
	defer stop()

	sp := spec.App{Name: app, Image: "nginx:alpine", Replicas: 1, Health: &spec.Health{
		HTTP:     &spec.HTTPCheck{Path: "/", Port: 80},
		Interval: spec.Duration(time.Second),
		Timeout:  spec.Duration(time.Second),
		Retries:  2,
	}}
	if _, err := st.PutApp(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	ag.Trigger()
	s := itWaitReplica(t, st, app, 90*time.Second, "healthy and ready", func(s store.Status) bool {
		return s.Health == "healthy" && s.Ready
	})
	if s.RestartCount != 0 {
		t.Fatalf("a healthy replica was restarted %d times", s.RestartCount)
	}
}

func TestIntegrationExecProbeHealthy(t *testing.T) {
	rt := itRuntime(t)
	st, app := itHealthSetup(t, rt)
	ag, stop := itAgent(t, rt, st)
	defer stop()

	sp := spec.App{Name: app, Image: "nginx:alpine", Replicas: 1, Health: &spec.Health{
		Exec:     []string{"test", "-d", "/usr/share/nginx/html"},
		Interval: spec.Duration(time.Second),
		Timeout:  spec.Duration(time.Second),
		Retries:  2,
	}}
	if _, err := st.PutApp(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	ag.Trigger()
	itWaitReplica(t, st, app, 90*time.Second, "exec probe healthy", func(s store.Status) bool {
		return s.Health == "healthy" && s.Ready
	})
}

// TestIntegrationCrashLoopBacksOff: a command that exits 1 at once is restarted
// on the backoff schedule (1 s, 2 s, 4 s, ...), not as fast as the agent can
// pass. Status shows the crash loop while a restart is scheduled.
func TestIntegrationCrashLoopBacksOff(t *testing.T) {
	rt := itRuntime(t)
	st, app := itHealthSetup(t, rt)
	ag, stop := itAgent(t, rt, st)
	defer stop()

	sp := spec.App{Name: app, Image: "nginx:alpine", Replicas: 1, Command: []string{"sh", "-c", "exit 1"}}
	if _, err := st.PutApp(context.Background(), sp); err != nil {
		t.Fatal(err)
	}
	ag.Trigger()

	deadline := time.Now().Add(40 * time.Second)
	sawCrashLoop, maxCount := false, 0
	for time.Now().Before(deadline) {
		ss, _ := st.ListStatus(context.Background(), app)
		if len(ss) == 1 {
			s := ss[0]
			if s.RestartCount > maxCount {
				maxCount = s.RestartCount
			}
			if s.CrashLooping && s.NextRestart.After(time.Now()) {
				if !sawCrashLoop {
					t.Logf("crash-looping, next restart in %s (restart count %d)",
						time.Until(s.NextRestart).Round(100*time.Millisecond), s.RestartCount)
				}
				sawCrashLoop = true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sawCrashLoop {
		t.Fatal("status never showed the crash loop with a restart scheduled")
	}
	// Schedule: restarts at about 1, 3, 7, 15 and 31 s. Five at most by 40 s.
	// A container restarted on every pass would be in the hundreds.
	if maxCount < 3 || maxCount > 6 {
		t.Fatalf("restart count %d after 40 s: want 3 to 6 on the 1, 2, 4 s schedule", maxCount)
	}
	t.Logf("crash loop: restart count %d after 40 s", maxCount)
}
