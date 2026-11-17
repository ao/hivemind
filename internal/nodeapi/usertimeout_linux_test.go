package nodeapi

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"
)

// Connections to the server carry TCP_USER_TIMEOUT, so one to a machine that
// stopped acknowledging fails within serverUserTimeout.
func TestServerConnectionsHaveAUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := NewClient("https://"+ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := c.tr.cur.Load()
	for _, tr := range []*http.Transport{tr, tr.Clone()} {
		conn, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := conn.(*net.TCPConn).SyscallConn()
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
		conn.Close()
		if gerr != nil || got != int(serverUserTimeout.Milliseconds()) {
			t.Fatalf("TCP_USER_TIMEOUT %d ms, %v; want %d", got, gerr, serverUserTimeout.Milliseconds())
		}
	}
}
