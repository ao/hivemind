package nodeapi

import "syscall"

// tcpUserTimeout is TCP_USER_TIMEOUT from linux/tcp.h, 18 on every Linux
// architecture; the syscall package does not define it on all of them.
const tcpUserTimeout = 0x12

// setUserTimeout sets TCP_USER_TIMEOUT (tcp(7)) on a connection to the
// server: data, or a keep-alive probe, that the server's machine has not
// acknowledged for serverUserTimeout fails the connection. A server machine
// that loses power, or is replaced at a new address, acknowledges nothing
// and sends no reset, so without it a request on a pooled connection to it
// waits out the whole client timeout, and a worker can miss the restored
// server's lease window. A server that is up acknowledges at once, however
// long it takes to answer, so the log poll's long requests are not cut.
func setUserTimeout(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout,
			int(serverUserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
