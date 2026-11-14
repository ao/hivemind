//go:build integration

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// A replica that allocates more than its memory limit is OOM-killed by the
// kernel, and the agent restarts it: the restart count goes up, and the
// agent's log says the exit was an OOM kill.
func TestIntegrationMemoryLimitOOMRestarted(t *testing.T) {
	rt := itRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var b [4]byte
	_, _ = rand.Read(b[:])
	appName := "oom-" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		cs, _ := rt.List(context.Background(), map[string]string{runtime.LabelApp: appName})
		for _, c := range cs {
			_ = rt.Remove(context.Background(), c.ID, true)
		}
	})

	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "local", CPU: 2, Memory: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	a, err := New(Config{NodeID: "node-1", Runtime: rt, Source: st, Interval: itInterval,
		Logger: slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logs), nil))})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopRun := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { a.Run(runCtx); close(done) }()
	defer func() { stopRun(); <-done }()

	// The shell reads 64 MiB into a variable, four times the 16Mi limit.
	app := spec.App{Name: appName, Image: "alpine:3",
		Command:   []string{"sh", "-c", `x=$(head -c 67108864 /dev/zero | tr '\0' x); echo survived ${#x}; sleep 300`},
		Resources: spec.Resources{CPU: 0.5, Memory: 16 << 20}}
	if _, err := st.PutApp(ctx, app); err != nil {
		t.Fatal(err)
	}
	a.Trigger()

	took := itWait(t, "an OOM-killed restart", 2*time.Minute, func() bool {
		a.Trigger()
		ss, _ := st.ListStatus(ctx, appName)
		return len(ss) == 1 && ss[0].RestartCount >= 2 && strings.Contains(logs.String(), "oom_killed=true")
	})
	t.Logf("two restarts after OOM kills within %s", took.Round(time.Millisecond))

	cs, err := rt.List(ctx, map[string]string{runtime.LabelApp: appName})
	if err != nil || len(cs) != 1 {
		t.Fatalf("containers: %+v %v", cs, err)
	}
	// The restarts happen in place: the one container is the generation 1
	// replica.
	if cs[0].Name != ContainerName(appName, 0, 1) {
		t.Fatalf("container %q, want the generation 1 replica", cs[0].Name)
	}
}
