package ha

import (
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT from linux/tcp.h.
const tcpUserTimeout = 0x12

// peerUserTimeout fails a server port connection whose data the other
// server's machine has not acknowledged for this long: a server that loses
// power acknowledges nothing and sends no reset, and a request passed to it
// would otherwise wait out its client's whole timeout.
const peerUserTimeout = 5 * time.Second

// setUserTimeout sets TCP_USER_TIMEOUT (tcp(7)) on a connection to another
// server, as the node API client does on its connections to the server.
func setUserTimeout(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(peerUserTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return serr
}
