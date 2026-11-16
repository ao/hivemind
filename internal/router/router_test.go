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

	"go.uber.org/zap"

	"github.com/ao/hivemind/internal/metrics"
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

func (tr *testRouter) metrics(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	w := metrics.NewWriter(&b)
	tr.rt.WriteMetrics(w)
	if err := w.Flush(); err != nil {
		t.Fatal(err)
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

func TestForwardedHeadersAreReplaced(t *testing.T) {
	up := upstream(t, "up")
	tr := newTestRouter(t, Table{"www.example.com": {up.Listener.Addr().String()}})
	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/x", nil)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Host", "evil.example")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("Forwarded", "for=6.6.6.6;host=evil.example")
	req.Header.Set("X-Real-Ip", "6.6.6.6")
	_, s, body := get(t, tr.client(false), req)
	h := s.Header
	if got := h.Values("X-Forwarded-For"); !slices.Equal(got, []string{"127.0.0.1"}) {
		t.Errorf("X-Forwarded-For %v: %s", got, body)
	}
	if got := h.Get("X-Forwarded-Host"); got != "www.example.com" {
		t.Errorf("X-Forwarded-Host %q", got)
	}
	if got := h.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto %q", got)
	}
	if h.Get("Forwarded") != "" || h.Get("X-Real-Ip") != "" {
		t.Errorf("client headers passed on: Forwarded %q, X-Real-Ip %q", h.Get("Forwarded"), h.Get("X-Real-Ip"))
	}
	if s.Host != "www.example.com" {
		t.Errorf("Host %q", s.Host)
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
	for reads() == 0 { // the poll's first read, at Serve
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
