package router

import (
	"context"
	"net"
	"syscall"
	"testing"
)

// Connections to replicas carry TCP_USER_TIMEOUT, so one to a machine that
// stopped acknowledging fails within upstreamUserTimeout.
func TestUpstreamConnectionsHaveAUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	e := newEndpoint(ln.Addr().String())
	c, err := e.tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
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
	if gerr != nil || got != int(upstreamUserTimeout.Milliseconds()) {
		t.Fatalf("TCP_USER_TIMEOUT %d ms, %v; want %d", got, gerr, upstreamUserTimeout.Milliseconds())
	}
}
