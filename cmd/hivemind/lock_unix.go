//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockFileName is the file in the data directory that serve and join lock.
const lockFileName = "hivemind.lock"

// lockDataDir takes an exclusive lock on the data directory dir, for serve
// and join: an flock on dir/hivemind.lock, which then holds this process's
// pid. The lock goes with the process, so a crash leaves none behind. A
// second serve or join on dir is refused with the pid that holds it, rather
// than starting a second agent beside the first. release drops the lock.
func lockDataDir(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("data directory: %w", err)
	}
	path := filepath.Join(dir, lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		defer f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			b, _ := io.ReadAll(io.LimitReader(f, 32))
			pid := strings.TrimSpace(string(b))
			if _, err := strconv.Atoi(pid); err != nil {
				pid = "unknown"
			}
			return nil, fmt.Errorf("another hivemind (pid %s) runs on the data directory %s; stop it first, or use another --data-dir", pid, dir)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock file: %w", err)
	}
	return func() { _ = f.Close() }, nil
}
