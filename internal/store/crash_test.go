package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// crashEnv names the store file a child process writes to until it is killed.
const crashEnv = "HIVEMIND_STORE_CRASH_CHILD"

func TestMain(m *testing.M) {
	if path := os.Getenv(crashEnv); path != "" {
		crashChild(path)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// crashChild changes the store in a loop, the way a busy server would, and
// prints "ready" once it has made some changes. It runs until it is killed.
func crashChild(path string) {
	ctx := context.Background()
	s, err := Open(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := s.UpsertNode(ctx, Node{ID: node1, Name: "one"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for i := 0; ; i++ {
		name := fmt.Sprintf("app-%d", i%5)
		var err error
		switch i % 7 {
		case 0, 1, 2:
			a := spec.App{Name: name, Image: "nginx:alpine", Replicas: 1 + i%4, Env: map[string]string{"V": fmt.Sprint(i % 3)}}
			_, err = s.PutApp(ctx, a)
		case 3:
			_, err = s.Scale(ctx, name, 1+i%5)
		case 4:
			err = s.DeleteApp(ctx, name)
		case 5:
			// Report a container for every assignment but the last, so some
			// removed replicas are purged and some are not.
			as, aerr := s.AssignmentsForNode(ctx, node1)
			if aerr != nil {
				err = aerr
				break
			}
			var st []Status
			for j, a := range as {
				if j == len(as)-1 {
					break
				}
				st = append(st, Status{Name: fmt.Sprintf("hivemind-%s-%d-g%d", a.App, a.Index, a.Generation),
					App: a.App, Replica: a.Index, Generation: a.Generation, State: "running"})
			}
			err = s.ReportStatus(ctx, node1, st)
		case 6:
			err = s.ReportStatus(ctx, node1, nil)
		}
		if err != nil && !isNotFound(err) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if i == 50 {
			fmt.Println("ready")
		}
	}
}

func isNotFound(err error) bool { return err != nil && errors.Is(err, ErrNotFound) }

// TestCrashConsistency kills a writing process with SIGKILL several times,
// and checks after each kill that the store opens and its invariants hold.
func TestCrashConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	path := filepath.Join(t.TempDir(), "data", "hivemind.db")
	for round := range 5 {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), crashEnv+"="+path)
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := make(chan bool, 1)
		go func() {
			line, _ := bufio.NewReader(out).ReadString('\n')
			ready <- line == "ready\n"
		}()
		select {
		case ok := <-ready:
			if !ok {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatalf("round %d: child exited before it was ready", round)
			}
		case <-time.After(30 * time.Second):
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("round %d: child not ready after 30s", round)
		}
		// Let it run a little longer, a different time each round, so the
		// kill lands at different points in a transaction.
		time.Sleep(time.Duration(20+37*round) * time.Millisecond)
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		err = cmd.Wait()
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("round %d: child did not die from SIGKILL: %v", round, err)
		}

		s, err := Open(ctxT(t), path)
		if err != nil {
			t.Fatalf("round %d: reopen after kill: %v", round, err)
		}
		if err := invariants(ctxT(t), s.db); err != nil {
			s.Close()
			t.Fatalf("round %d: %v", round, err)
		}
		apps, err := s.ListApps(ctxT(t))
		s.Close()
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		t.Logf("round %d: consistent after SIGKILL, %d apps", round, len(apps))
	}
}
