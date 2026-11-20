//go:build !linux

package ha

import "syscall"

// setUserTimeout does nothing where TCP_USER_TIMEOUT does not exist.
// Servers run on Linux.
func setUserTimeout(_, _ string, _ syscall.RawConn) error { return nil }
