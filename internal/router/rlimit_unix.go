//go:build linux || darwin

package router

import "syscall"

// openFileLimit is the process's soft RLIMIT_NOFILE, which Go raises to the
// hard limit at start, or 0 if it cannot be read.
func openFileLimit() int {
	var l syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l); err != nil || l.Cur > 1<<30 {
		return 0
	}
	return int(l.Cur)
}
