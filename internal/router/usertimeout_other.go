//go:build !linux

package router

import "syscall"

// setUserTimeout does nothing where TCP_USER_TIMEOUT does not exist. A node
// runs on Linux; the router builds elsewhere only for the client binaries.
func setUserTimeout(_, _ string, _ syscall.RawConn) error { return nil }
