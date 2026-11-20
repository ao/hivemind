//go:build linux

package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// blackhole returns the address of a listener whose accept queue is full
// and never accepted from, so that the kernel drops a SYN to it, as the
// network does one to a machine that is gone: a connect to it waits.
func blackhole(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", sa.(*syscall.SockaddrInet4).Port)
	// Fill the queue: once it is full, a connect times out.
	for i := 0; ; i++ {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err != nil {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				t.Fatalf("filling the accept queue: %v", err)
			}
			return addr
		}
		t.Cleanup(func() { c.Close() })
		if i == 16 {
			t.Skip("the kernel took 16 connections into an accept queue of backlog 0")
		}
	}
}

// TestCLIPassesADeadServerQuickly: with several servers given, a server
// that does not answer a SYN costs a call 2 s, not the 30 s of one
// server's connect, before the next answers (the eighth soak's first
// finding); and the next call starts at the server that answered, which
// the CLI keeps in a file of the user's own.
func TestCLIPassesADeadServerQuickly(t *testing.T) {
	home := isolate(t)
	dead := blackhole(t)
	var hits atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"leader":"node-b","answered_by":"node-b","servers":[{"id":"node-b","advertise":"10.0.0.6","node_port":4484,"role":"leader"}]}`))
	}))
	defer good.Close()
	bases := []string{"http://" + dead, good.URL}
	list := strings.Join(bases, ",")

	start := time.Now()
	out, _, err := run(t, "", "servers", "--server", list, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("with the first server gone, the call took %s", d)
	}
	if !strings.Contains(out, "node-b") || hits.Load() != 1 {
		t.Fatalf("the second server did not answer: %d requests, output %q", hits.Load(), out)
	}

	p, err := lastServerPath(bases)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p, filepath.Join(home, ".cache", "hivemind")+string(filepath.Separator)) {
		t.Fatalf("the last server's file is %s, outside the user's cache directory", p)
	}
	if m := mode(t, p); m != 0o600 {
		t.Fatalf("%s has mode %v", p, m)
	}
	if m := mode(t, filepath.Dir(p)); m != 0o700 {
		t.Fatalf("%s has mode %v", filepath.Dir(p), m)
	}

	start = time.Now()
	if _, _, err := run(t, "", "servers", "--server", list, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the next call took %s: it did not start at the server that answered", d)
	}

	// Only a server of the list is taken from the file; another list has
	// a file of its own.
	if err := os.WriteFile(p, []byte("http://10.9.9.9:4483\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastServer(bases); got != "" {
		t.Fatalf("a file naming a server outside the list gave %q", got)
	}
	if other, _ := lastServerPath([]string{good.URL, "http://" + dead}); other == p {
		t.Fatal("two orders of the list share one file")
	}
}
