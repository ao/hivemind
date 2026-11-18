package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// The HTTP probe speaks HTTP/1.1, so it fails against a server that speaks
// only HTTP/2 without TLS, as a gRPC server does: such a server closes the
// connection on a request that does not start with the HTTP/2 preface. An
// app behind an h2c route whose server speaks only h2c uses a TCP check, or
// an exec check such as grpc_health_probe, instead (docs/routing.md). A
// server that speaks both passes the HTTP probe.
func TestHTTPProbeAndH2COnlyServers(t *testing.T) {
	serve := func(h1 bool) (string, int) {
		s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		s.Config.Protocols = new(http.Protocols)
		s.Config.Protocols.SetUnencryptedHTTP2(true)
		s.Config.Protocols.SetHTTP1(h1)
		s.Start()
		t.Cleanup(s.Close)
		host, port, _ := net.SplitHostPort(s.Listener.Addr().String())
		n, _ := strconv.Atoi(port)
		return host, n
	}
	ctx := context.Background()
	ip, port := serve(false)
	if err := Check(ctx, httpHealth(port, "/", time.Second), Target{IP: ip}, nil); err == nil {
		t.Fatal("the HTTP probe passed against a server that speaks only h2c")
	} else {
		t.Logf("HTTP probe against an h2c-only server: %v", err)
	}
	tcp := spec.Health{TCP: &spec.TCPCheck{Port: port}, Timeout: spec.Duration(time.Second)}
	if err := Check(ctx, tcp, Target{IP: ip}, nil); err != nil {
		t.Fatalf("the TCP probe against an h2c-only server: %v", err)
	}
	ip, port = serve(true)
	if err := Check(ctx, httpHealth(port, "/", time.Second), Target{IP: ip}, nil); err != nil {
		t.Fatalf("the HTTP probe against a server that speaks HTTP/1.1 and h2c: %v", err)
	}
}
