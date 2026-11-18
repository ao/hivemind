package router

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// Connections to replicas carry TCP_USER_TIMEOUT, so one to a machine that
// stopped acknowledging fails within upstreamUserTimeout, over HTTP/1.1 and
// over h2c alike.
func TestUpstreamConnectionsHaveAUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, h2c := range []bool{false, true} {
		e := newEndpoint(ln.Addr().String(), h2c, upstreamResponseTimeout)
		c, err := e.tr.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := c.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var got int
		var gerr error
		if err := raw.Control(func(fd uintptr) {
			got, gerr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout)
		}); err != nil {
			t.Fatal(err)
		}
		c.Close()
		if gerr != nil || got != int(upstreamUserTimeout.Milliseconds()) {
			t.Fatalf("h2c=%v: TCP_USER_TIMEOUT %d ms, %v; want %d", h2c, got, gerr, upstreamUserTimeout.Milliseconds())
		}
	}
}
