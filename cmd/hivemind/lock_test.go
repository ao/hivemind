//go:build unix

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// lockHelperEnv makes the test binary a helper process that holds the lock
// on the directory it names until its standard input closes.
const lockHelperEnv = "HIVEMIND_TEST_LOCK_HELPER_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(lockHelperEnv); dir != "" {
		release, err := lockDataDir(dir)
		if err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		release()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Two processes on one data directory: while one holds it, serve, join and
// serve --role worker in another are refused with the holder's pid, before
// they touch anything. Once the holder exits, the directory can be locked.
func TestLockDataDirTwoProcesses(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	helper := exec.Command(os.Args[0], "-test.run=^$")
	helper.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		_ = helper.Wait()
	}()
	lines := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(stdout).ReadString('\n')
		lines <- l
	}()
	select {
	case l := <-lines:
		if l != "locked\n" {
			t.Fatalf("helper: %q", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the helper did not take the lock")
	}

	want := fmt.Sprintf("another hivemind (pid %d) runs on the data directory %s", helper.Process.Pid, dir)
	if _, err := lockDataDir(dir); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a second lock: %v, want %q", err, want)
	}
	for _, args := range [][]string{
		{"serve", "--data-dir", dir},
		{"serve", "--role", "worker", "--data-dir", dir},
		{"join", "127.0.0.1", "--token", "hivemind-join-v1.x", "--data-dir", dir},
	} {
		if _, _, err := run(t, "", args...); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v, want %q", args, err, want)
		}
	}
	if _, err := os.Stat(dir + "/" + storeFileName); err == nil {
		t.Error("a refused serve created the store")
	}

	stdin.Close()
	if err := helper.Wait(); err != nil {
		t.Fatalf("helper: %v", err)
	}
	release, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after the holder exited: %v", err)
	}
	release()
}
