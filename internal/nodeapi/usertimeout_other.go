//go:build !linux

package nodeapi

import "syscall"

// setUserTimeout does nothing where TCP_USER_TIMEOUT does not exist. Workers
// run on Linux.
func setUserTimeout(_, _ string, _ syscall.RawConn) error { return nil }
