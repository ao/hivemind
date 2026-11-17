package router

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"

	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

var testHosts = []string{"www.example.com", "api.example.com", "post.example.com", "big.example.com"}

type testRouter struct {
	rt    *Router
	st    *store.Store
	pool  *x509.CertPool
	logs  *syncBuffer
	table func() Table
	reads int // calls of routes, under mu
	mu    sync.Mutex
	err   error
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newTestRouter starts a router on loopback with a self-signed certificate
// for testHosts in its cache, and no ACME: certificate management is a no-op.
func newTestRouter(t *testing.T, table Table) *testRouter {
	t.Helper()
	return newTestRouterPoll(t, table, 20*time.Millisecond)
}

// newTestRouterPoll is newTestRouter with the table read every poll.
func newTestRouterPoll(t *testing.T, table Table, poll time.Duration) *testRouter {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ts, err := tlsstore.New(tlsstore.StoreBackend{St: st, NodeID: "node-a"}, tlsstore.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	tr := &testRouter{st: st, logs: logs}
	tr.table = func() Table { return table }
	rt, err := New(Config{Addr: "127.0.0.1", Routes: tr.routes, Storage: ts, Email: "ops@example.com",
		CA: "https://acme.invalid/dir", Logger: log, Metrics: NewMetrics(), Poll: poll})
	if err != nil {
		t.Fatal(err)
	}
	rt.manage = func(context.Context, []string) error { return nil }
	cert, leaf := selfSigned(t, testHosts...)
	if _, err := rt.magic.CacheUnmanagedTLSCertificate(context.Background(), cert, nil); err != nil {
		t.Fatal(err)
	}
	rt.SetTable(table)
	go func() { _ = rt.Serve() }()
	t.Cleanup(func() { _ = rt.Shutdown() })
	tr.rt = rt
	tr.pool = x509.NewCertPool()
	tr.pool.AddCert(leaf)
	return tr
}

func (tr *testRouter) routes(context.Context) (Table, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.reads++
	return tr.table(), tr.err
}

func (tr *testRouter) setRoutes(t Table, err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.table = func() Table { return t }
	tr.err = err
}

// client dials the router's https listener for every address, and speaks
// HTTP/2 unless h1 is set.
func (tr *testRouter) client(h1 bool) *http.Client {
	addr := tr.rt.HTTPSAddr()
	t := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: tr.pool},
		ForceAttemptHTTP2: !h1,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}
	if h1 {
		t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{Transport: t, Timeout: 10 * time.Second}
}

// plain dials the router's http listener for every address and does not
// follow redirects.
func (tr *testRouter) plain() *http.Client {
	addr := tr.rt.HTTPAddr()
	return &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

// metrics is the router's report, checked as the server checks it, one
// series a line in the form /metrics shows it, without the node label.
func (tr *testRouter) metrics(t *testing.T) string {
	t.Helper()
	r := tr.rt.Report()
	if err := r.Validate(); err != nil {
		t.Fatalf("the router's report is invalid: %v", err)
	}
	var b strings.Builder
	for _, x := range r.Requests {
		fmt.Fprintf(&b, "hivemind_router_requests_total{host=%q,code=\"%d\"} %d\n", x.Host, x.Code, x.Count)
	}
	for _, x := range r.Upstream {
		fmt.Fprintf(&b, "hivemind_router_upstream_errors_total{host=%q,kind=%q} %d\n", x.Host, x.Kind, x.Count)
	}
	for _, x := range r.Certs {
		fmt.Fprintf(&b, "hivemind_router_cert_expiry_timestamp_seconds{host=%q} %d\n", x.Host, x.NotAfter.Unix())
	}
	return b.String()
}

func selfSigned(t *testing.T, names ...string) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: names, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

// upstream is a replica: it answers with its name and what it received.
type seen struct {
	Name   string
	Host   string
	Header http.Header
	Body   int
}

func upstream(t *testing.T, name string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(seen{Name: name, Host: r.Host, Header: r.Header, Body: int(n)})
	}))
	t.Cleanup(s.Close)
	return s
}

// deadAddr is an address that refuses connections.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func get(t *testing.T, c *http.Client, req *http.Request) (*http.Response, seen, string) {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var s seen
	_ = json.Unmarshal(b, &s)
	return resp, s, string(b)
}

func TestRoundRobinFailoverAndHTTP2(t *testing.T) {
	up1, up2 := upstream(t, "up1"), upstream(t, "up2")
	dead := deadAddr(t)
	tr := newTestRouter(t, Table{"www.example.com": {up1.Listener.Addr().String(), dead, up2.Listener.Addr().String()}})
	c := tr.client(false)

	names := map[string]int{}
	for range 9 {
		req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
		resp, s, body := get(t, c, req)
		if resp.StatusCode != http.StatusOK || resp.ProtoMajor != 2 {
			t.Fatalf("status %d over HTTP/%d: %s", resp.StatusCode, resp.ProtoMajor, body)
		}
		names[s.Name]++
	}
	if names["up1"] == 0 || names["up2"] == 0 || names["up1"]+names["up2"] != 9 {
		t.Fatalf("replies by replica: %v", names)
	}
	// The dead endpoint failed one dial and was then skipped.
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_upstream_errors_total{host="www.example.com",kind="dial"} 1`) {
		t.Fatalf("dial errors:\n%s", m)
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_requests_total{host="www.example.com",code="200"} 9`) {
		t.Fatalf("requests:\n%s", m)
	}

	// Every endpoint down: 502.
	tr.rt.SetTable(Table{"www.example.com": {dead}})
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("all endpoints dead: %d", resp.StatusCode)
	}
}

// A request with a body whose first replica cannot be dialled goes to the
// next one with its whole body.
func TestFailoverKeepsTheBody(t *testing.T) {
	up := upstream(t, "up")
	tr := newTestRouter(t, Table{"post.example.com": {deadAddr(t), up.Listener.Addr().String()}})
	body := strings.Repeat("x", 100<<10)
	for _, h1 := range []bool{true, false} {
		req, _ := http.NewRequest(http.MethodPost, "https://post.example.com/upload", strings.NewReader(body))
		resp, s, raw := get(t, tr.client(h1), req)
		if resp.StatusCode != http.StatusOK || s.Body != len(body) {
			t.Fatalf("h1=%v: status %d, replica read %d bytes: %s", h1, resp.StatusCode, s.Body, raw)
		}
	}
}

// The replica gets no forwarding header the client chose: every one the
// client sends is removed, in any case and with "_" for "-", and the router
// sets its own. Host and X-Forwarded-Host are the table's host, whatever case,
// trailing dot or port the client wrote.
func TestForwardedHeadersAreReplaced(t *testing.T) {
	up := upstream(t, "up")
	tr := newTestRouter(t, Table{"www.example.com": {up.Listener.Addr().String()}})
	authority := fmt.Sprintf("www.example.com:%d", tr.rt.HTTPSPort())
	sent := map[string]string{
		"X-Forwarded-For": "6.6.6.6", "X-Forwarded-Host": "evil.example", "X-Forwarded-Proto": "http",
		"X-Forwarded-Port": "80", "X-Forwarded-Prefix": "/admin", "X-Forwarded-Ssl": "off",
		"X-Forwarded-Server": "evil.example", "X-Forwarded-Uri": "/admin", "Forwarded": "for=6.6.6.6;host=evil.example",
		"X-Real-Ip": "6.6.6.6", "True-Client-Ip": "6.6.6.6", "X-Client-Ip": "6.6.6.6",
		"X-Cluster-Client-Ip": "6.6.6.6", "Cf-Connecting-Ip": "6.6.6.6",
		"X_Forwarded_For": "6.6.6.6", "x-real_ip": "6.6.6.6",
	}
	for _, h1 := range []bool{true, false} {
		req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/x", nil)
		req.Host = "WWW.Example.COM.:1234"
		for k, v := range sent {
			req.Header[k] = []string{v} // as written, not canonicalised
		}
		resp, s, body := get(t, tr.client(h1), req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("h1=%v: %d %s", h1, resp.StatusCode, body)
		}
		want := map[string]string{
			"X-Forwarded-For": "127.0.0.1", "X-Forwarded-Host": authority, "X-Forwarded-Proto": "https",
			"X-Forwarded-Port": fmt.Sprint(tr.rt.HTTPSPort()),
		}
		for k, v := range want {
			if got := s.Header.Values(k); !slices.Equal(got, []string{v}) {
				t.Errorf("h1=%v: %s %q, want %q", h1, k, got, v)
			}
		}
		for k, v := range s.Header {
			if _, set := want[k]; isClientHeader(k) && !set {
				t.Errorf("h1=%v: the client's %s %q reached the replica", h1, k, v)
			}
		}
		if s.Host != authority {
			t.Errorf("h1=%v: Host %q, want %q", h1, s.Host, authority)
		}
	}
}

// switcher is a replica that switches protocols when a request asks, and
// then echoes what it reads; with h2c set, it switches to h2c whatever the
// request asks, and closes. A request that asks for nothing gets what it
// sent, as upstream answers.
func switcher(t *testing.T, h2c bool) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto := r.Header.Get("Upgrade")
		if h2c {
			proto = "h2c"
		}
		if proto == "" {
			_ = json.NewEncoder(w).Encode(seen{Name: "switcher", Host: r.Host, Header: r.Header})
			return
		}
		c, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		fmt.Fprintf(brw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n\r\n", proto)
		if brw.Flush() != nil || h2c {
			return
		}
		buf := make([]byte, 64)
		for {
			n, err := brw.Read(buf)
			if err != nil {
				return
			}
			_, _ = brw.Write(buf[:n])
			if brw.Flush() != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Close)
	return s.Listener.Addr().String()
}

// upgrade sends an HTTP/1.1 request that asks to switch to protos.
func upgrade(t *testing.T, c *http.Client, host string, protos string) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/", nil)
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req.Header.Set("Upgrade", protos)
	req.Header.Set("HTTP2-Settings", "AAMAAABkAARAAAAAAAIAAAAA")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("upgrade to %s: %v", protos, err)
	}
	return resp
}

// An h2c upgrade does not reach the replica: the router drops the token, and
// the request goes on as HTTP/1.1. A replica that switched to h2c would speak
// HTTP/2 with the client directly, past the router. WebSocket upgrades pass,
// alone or beside h2c.
func TestH2CUpgradeIsDroppedAndWebSocketsWork(t *testing.T) {
	tr := newTestRouter(t, Table{"www.example.com": {switcher(t, false)}, "api.example.com": {switcher(t, true)}})
	c := tr.client(true)

	resp := upgrade(t, c, "www.example.com", "h2c")
	if resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body.Close()
		t.Fatalf("the router switched the connection to %q with the replica", resp.Header.Get("Upgrade"))
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var s seen
	_ = json.Unmarshal(b, &s)
	if resp.StatusCode != http.StatusOK || s.Name != "switcher" {
		t.Fatalf("h2c upgrade: %d %q, want 200 from the replica as plain HTTP/1.1", resp.StatusCode, b)
	}
	for _, k := range []string{"Upgrade", "Connection", "Http2-Settings"} {
		if v := s.Header.Values(k); len(v) > 0 {
			t.Errorf("the replica got %s %q", k, v)
		}
	}

	// A replica that switches to h2c though the request no longer asks gets
	// the client a 502, not a 101.
	resp = upgrade(t, c, "api.example.com", "h2c")
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("a replica that switched to h2c unasked: %d, want 502", resp.StatusCode)
	}

	// A client with a timeout wraps the body, which then cannot be written.
	ws := *c
	ws.Timeout = 0
	for _, protos := range []string{"websocket", "h2c, websocket"} {
		resp = upgrade(t, &ws, "www.example.com", protos)
		if resp.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
			resp.Body.Close()
			t.Fatalf("Upgrade: %s: %d %q, want 101 websocket", protos, resp.StatusCode, resp.Header.Get("Upgrade"))
		}
		rwc := resp.Body.(io.ReadWriteCloser)
		if _, err := rwc.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(rwc, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("Upgrade: %s: echo %q, %v", protos, buf, err)
		}
		rwc.Close()
	}
}

func TestAuthorityHasThePortOnlyWhenNot443(t *testing.T) {
	for port, want := range map[int]string{443: "www.example.com", 8443: "www.example.com:8443"} {
		if got := (&proxy{httpsPort: port}).authority("www.example.com"); got != want {
			t.Errorf("https port %d: %q, want %q", port, got, want)
		}
	}
}

func TestUnknownHostsAndHostsWithoutReplicas(t *testing.T) {
	tr := newTestRouter(t, Table{"www.example.com": {}, "api.example.com": {deadAddr(t)}})
	c := tr.client(true)

	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("host without a ready replica: %d %s", resp.StatusCode, body)
	}
	// A Host header for another name than the SNI is misdirected.
	req, _ = http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	req.Host = "api.example.com"
	if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("Host unlike SNI: %d", resp.StatusCode)
	}
	// A name the table does not have gets no certificate.
	_, err := tls.Dial("tcp", tr.rt.HTTPSAddr(), &tls.Config{RootCAs: tr.pool, ServerName: "nope.example.com"})
	if err == nil {
		t.Fatal("a handshake for an unknown name succeeded")
	}
	// Over HTTP, an unknown host is a plain 404 that lists nothing.
	req, _ = http.NewRequest(http.MethodGet, "http://nope.example.com/", nil)
	resp, _, body := get(t, tr.plain(), req)
	if resp.StatusCode != http.StatusNotFound || body != "not found\n" {
		t.Fatalf("unknown host over http: %d %q", resp.StatusCode, body)
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_requests_total{host="unknown",code="404"} 1`) {
		t.Fatalf("unknown host metrics:\n%s", m)
	}
}

func TestHTTPRedirectsAndAnswersChallenges(t *testing.T) {
	tr := newTestRouter(t, Table{"www.example.com": {}})
	c := tr.plain()
	req, _ := http.NewRequest(http.MethodGet, "http://www.example.com/a/b?c=d", nil)
	resp, _, _ := get(t, c, req)
	want := fmt.Sprintf("https://www.example.com:%d/a/b?c=d", tr.rt.HTTPSPort())
	if resp.StatusCode != http.StatusPermanentRedirect || resp.Header.Get("Location") != want {
		t.Fatalf("redirect: %d %q, want 308 %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}

	// A challenge another router started, from the shared storage. The key
	// is where certmagic's distributed solver writes it.
	chal := `{"type":"http-01","url":"https://acme.invalid/chal/1","status":"pending","token":"tok_123",` +
		`"keyAuthorization":"tok_123.thumbprint","identifier":{"type":"dns","value":"www.example.com"}}`
	if err := tr.st.TLSStore(context.Background(), "acme/acme.invalid-dir/challenge_tokens/www.example.com.json", []byte(chal)); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest(http.MethodGet, "http://www.example.com/.well-known/acme-challenge/tok_123", nil)
	resp, _, body := get(t, c, req)
	if resp.StatusCode != http.StatusOK || body != "tok_123.thumbprint" {
		t.Fatalf("challenge: %d %q", resp.StatusCode, body)
	}
	// Another token for the host is not answered with this one: it is
	// redirected like any other path.
	req, _ = http.NewRequest(http.MethodGet, "http://www.example.com/.well-known/acme-challenge/other", nil)
	if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("another token: %d", resp.StatusCode)
	}
	// A challenge path for an unknown host is a 404, and storage is not read.
	req, _ = http.NewRequest(http.MethodGet, "http://nope.example.com/.well-known/acme-challenge/tok_123", nil)
	if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("challenge for an unknown host: %d", resp.StatusCode)
	}
}

func TestHeaderLimits(t *testing.T) {
	bigHeaders := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Big", strings.Repeat("y", 100<<10))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(bigHeaders.Close)
	up := upstream(t, "up")
	tr := newTestRouter(t, Table{"www.example.com": {up.Listener.Addr().String()}, "big.example.com": {bigHeaders.Listener.Addr().String()}})

	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	req.Header.Set("X-Big", strings.Repeat("x", 100<<10))
	if resp, _, _ := get(t, tr.client(true), req); resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("request headers over the limit: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://big.example.com/", nil)
	if resp, _, _ := get(t, tr.client(false), req); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("response headers over the limit: %d", resp.StatusCode)
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_upstream_errors_total{host="big.example.com",kind="proxy"} 1`) {
		t.Fatalf("proxy errors:\n%s", m)
	}
}

// The table is read on a timer; a failed read keeps the last table; hosts
// that join are managed, and hosts that leave are not.
func TestTablePollAndManagedHosts(t *testing.T) {
	up := upstream(t, "up")
	tr := newTestRouter(t, Table{"www.example.com": {up.Listener.Addr().String()}})
	var mu sync.Mutex
	var managed [][]string
	tr.rt.mu.Lock()
	tr.rt.manage = func(_ context.Context, hosts []string) error {
		mu.Lock()
		defer mu.Unlock()
		managed = append(managed, slices.Clone(hosts))
		return nil
	}
	tr.rt.mu.Unlock()
	tr.setRoutes(Table{"www.example.com": {up.Listener.Addr().String()}, "api.example.com": {up.Listener.Addr().String()}}, nil)
	waitFor(t, func() bool { return slices.Equal(tr.rt.Managed(), []string{"api.example.com", "www.example.com"}) })
	mu.Lock()
	if len(managed) != 1 || !slices.Equal(managed[0], []string{"api.example.com"}) {
		t.Fatalf("managed calls: %v", managed)
	}
	mu.Unlock()

	tr.setRoutes(nil, errors.New("server unreachable"))
	time.Sleep(100 * time.Millisecond)
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/", nil)
	if resp, _, _ := get(t, tr.client(false), req); resp.StatusCode != http.StatusOK {
		t.Fatalf("after a failed read the last table is gone: %d", resp.StatusCode)
	}
	if !strings.Contains(tr.logs.String(), "serving the last one") {
		t.Error("a failed read was not logged")
	}

	tr.setRoutes(Table{"api.example.com": {up.Listener.Addr().String()}}, nil)
	waitFor(t, func() bool { return slices.Equal(tr.rt.Managed(), []string{"api.example.com"}) })
	req, _ = http.NewRequest(http.MethodGet, "https://api.example.com/", nil)
	req.Host = "api.example.com"
	if resp, _, _ := get(t, tr.client(false), req); resp.StatusCode != http.StatusOK {
		t.Fatalf("api after www left: %d", resp.StatusCode)
	}
	if _, err := tls.Dial("tcp", tr.rt.HTTPSAddr(), &tls.Config{RootCAs: tr.pool, ServerName: "www.example.com"}); err == nil {
		t.Fatal("a host that left the table still gets a certificate")
	}
}

func TestPoolOrder(t *testing.T) {
	now := time.Unix(1000, 0)
	a, b, c := newEndpoint("a:1"), newEndpoint("b:1"), newEndpoint("c:1")
	p := &pool{endpoints: []*endpoint{a, b, c}}
	var firsts []string
	for range 3 {
		firsts = append(firsts, p.order(now)[0].addr)
	}
	if !slices.Equal(firsts, []string{"a:1", "b:1", "c:1"}) {
		t.Fatalf("round robin: %v", firsts)
	}
	b.downUntil.Store(now.Add(time.Second).UnixNano())
	order := p.order(now)
	if got := []string{order[0].addr, order[1].addr, order[2].addr}; !slices.Equal(got, []string{"a:1", "c:1", "b:1"}) {
		t.Fatalf("a down endpoint goes last: %v", got)
	}
	if got := p.order(now.Add(2 * time.Second)); len(got) != 3 || got[0].addr != "b:1" {
		t.Fatalf("an endpoint is tried again once its mark ends: %v", got[0].addr)
	}
}

func TestZapLinesReachSlog(t *testing.T) {
	var b syncBuffer
	z := newZap(slog.New(slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelInfo})))
	z.Named("obtain").With(zap.String("identifier", "www.example.com")).Info("lock acquired", zap.Int("attempt", 2))
	z.Debug("not shown")
	got := b.String()
	for _, want := range []string{"level=INFO", `msg="lock acquired"`, "logger=obtain", "identifier=www.example.com", "attempt=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in %q", want, got)
		}
	}
	if strings.Contains(got, "not shown") {
		t.Error("a debug line passed an info handler")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(end) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A CA can validate through any router, and the router that ordered a
// certificate may have read the table with the new host before this one did.
// A challenge for a host the router does not know yet makes it read the table
// at once, rather than answer 404; challenges for made-up hosts read it at
// most once a second.
func TestChallengeForAHostNotReadYet(t *testing.T) {
	tr := newTestRouterPoll(t, Table{}, time.Hour)
	reads := func() int {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		return tr.reads
	}
	deadline := time.Now().Add(5 * time.Second)
	for reads() < 2 { // the start's read and the poll's first one
		if time.Now().After(deadline) {
			t.Fatal("the router never read the table")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The server's table has the host now; the poll will not read it for an hour.
	tr.setRoutes(Table{"www.example.com": {}}, nil)
	chal := `{"type":"http-01","url":"https://acme.invalid/chal/1","status":"pending","token":"tok_9",` +
		`"keyAuthorization":"tok_9.thumbprint","identifier":{"type":"dns","value":"www.example.com"}}`
	if err := tr.st.TLSStore(context.Background(), "acme/acme.invalid-dir/challenge_tokens/www.example.com.json", []byte(chal)); err != nil {
		t.Fatal(err)
	}
	c := tr.plain()
	req, _ := http.NewRequest(http.MethodGet, "http://www.example.com/.well-known/acme-challenge/tok_9", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusOK || body != "tok_9.thumbprint" {
		t.Fatalf("challenge for a host the router had not read: %d %q, want 200 and the key authorization", resp.StatusCode, body)
	}

	// Within the next second, challenges for made-up hosts cause no read.
	before := reads()
	for i := range 5 {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://made-up-%d.example.com/.well-known/acme-challenge/x", i), nil)
		if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("challenge for a made-up host: %d, want 404", resp.StatusCode)
		}
	}
	if n := reads() - before; n > 1 {
		t.Fatalf("%d table reads for 5 challenges for made-up hosts in a second, want at most 1", n)
	}
	// Other paths for an unknown host read nothing.
	before = reads()
	req, _ = http.NewRequest(http.MethodGet, "http://made-up.example.com/", nil)
	if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusNotFound || reads() != before {
		t.Fatalf("a plain request for an unknown host: %d, %d reads", resp.StatusCode, reads()-before)
	}
}

// A router that starts loads the certificates the shared storage already has
// for its hosts before it takes TLS connections. The router soak saw the
// first handshakes after each start fail, while the table and the
// certificates were still loading.
func TestStartLoadsStoredCertificatesBeforeTLS(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts, err := tlsstore.New(tlsstore.StoreBackend{St: st, NodeID: "node-a"}, tlsstore.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	// The table is slow to read, as over the node API right after a start.
	routes := func(ctx context.Context) (Table, error) {
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return Table{"www.example.com": {}}, nil
	}
	rt, err := New(Config{Addr: "127.0.0.1", Routes: routes, Storage: ts, Email: "ops@example.com",
		CA: "https://acme.invalid/dir", Logger: log, Metrics: NewMetrics(), Poll: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	rt.manage = func(context.Context, []string) error { return nil } // no ACME here

	// Another router obtained the certificate earlier: it is in storage.
	cert, leaf := selfSigned(t, "www.example.com")
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	issuer := rt.issuer.IssuerKey()
	for key, val := range map[string][]byte{
		certmagic.StorageKeys.SiteCert(issuer, "www.example.com"):       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		certmagic.StorageKeys.SitePrivateKey(issuer, "www.example.com"): pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		certmagic.StorageKeys.SiteMeta(issuer, "www.example.com"):       []byte(`{"sans":["www.example.com"],"issuer_data":null}`),
	} {
		if err := ts.Store(context.Background(), key, val); err != nil {
			t.Fatal(err)
		}
	}
	go func() { _ = rt.Serve() }()
	t.Cleanup(func() { _ = rt.Shutdown() })

	// The first handshake, made at once, succeeds with the stored certificate.
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", rt.HTTPSAddr(),
		&tls.Config{RootCAs: pool, ServerName: "www.example.com"})
	if err != nil {
		t.Fatalf("the first handshake after the start: %v", err)
	}
	defer conn.Close()
	if got := conn.ConnectionState().PeerCertificates[0]; !got.Equal(leaf) {
		t.Fatal("the router served another certificate than the stored one")
	}
}

// resetter is a replica whose connections fail after the request is sent:
// it reads the request and resets the connection, with no reply, as a
// connection to a machine that lost power fails once TCP_USER_TIMEOUT ends
// it.
func resetter(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0) // a reset, not a clean close
				}
				c.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

// A request that can be sent twice goes to the next replica when the first
// one's connection fails before it replies; one with a body, or a method
// that is not safe to repeat, gets 502. The router soak saw a request on a
// connection to a machine that had just lost power wait for its timeout.
func TestRetryWhenAConnectionFailsBeforeTheReply(t *testing.T) {
	up := upstream(t, "up")
	tr := newTestRouter(t, Table{"www.example.com": {resetter(t), up.Listener.Addr().String()}})
	c := tr.client(true)
	for i := range 4 {
		req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
		if resp, _, _ := get(t, c, req); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %d: %d, want 200 from the replica that answers", i, resp.StatusCode)
		}
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_upstream_errors_total{host="www.example.com",kind="connection"}`) {
		t.Fatalf("no connection error counted:\n%s", m)
	}
	// A fresh router: the GETs marked the failing replica down for a while.
	tr = newTestRouter(t, Table{"www.example.com": {resetter(t), up.Listener.Addr().String()}})
	c = tr.client(true)
	bad := 0
	for range 4 {
		req, _ := http.NewRequest(http.MethodPost, "https://www.example.com/", strings.NewReader("body"))
		resp, _, _ := get(t, c, req)
		if resp.StatusCode == http.StatusBadGateway {
			bad++
		}
	}
	if bad == 0 {
		t.Fatal("a POST was retried on another replica after the first one's connection failed")
	}
}
