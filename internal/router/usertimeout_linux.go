package router

import "syscall"

// tcpUserTimeout is TCP_USER_TIMEOUT from linux/tcp.h, 18 on every Linux
// architecture. The frozen syscall package defines it on some (arm64) and
// not on others (amd64).
const tcpUserTimeout = 0x12

// setUserTimeout sets TCP_USER_TIMEOUT (tcp(7)) on an upstream connection:
// data the replica's machine has not acknowledged for upstreamUserTimeout
// fails the connection. A machine that loses power acknowledges nothing and
// sends no reset, so without it a request sent on a kept connection to it
// waits for the response timeout. A machine that is up acknowledges at once,
// however slow its app, so slow replicas are not cut off.
func setUserTimeout(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout,
			int(upstreamUserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
