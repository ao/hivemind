package router

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests here route a host's requests over h2c to replicas that speak
// HTTP/2 without TLS and nothing else, as a gRPC server does, with gRPC's
// framing: each message is a flag byte, a 4-byte big-endian length and the
// message, and the call's status is in the trailers.

func grpcFrame(msg string) []byte {
	b := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:], msg)
	return b
}

// readFrame reads one message; io.EOF is a clean end between messages.
func readFrame(r io.Reader) (string, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > 1<<20 {
		return "", fmt.Errorf("a message of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

// sendFrame writes one message to the client and flushes it.
func sendFrame(w http.ResponseWriter, msg string) error {
	if _, err := w.Write(grpcFrame(msg)); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

// grpcStatus sets the call's status as undeclared trailers, as gRPC servers
// do.
func grpcStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set(http.TrailerPrefix+"Grpc-Status", strconv.Itoa(code))
	w.Header().Set(http.TrailerPrefix+"Grpc-Message", msg)
}

// h2cServer starts a replica that speaks HTTP/2 with prior knowledge and no
// HTTP/1.1, and returns its address.
func h2cServer(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewUnstartedServer(h)
	s.Config.Protocols = new(http.Protocols)
	s.Config.Protocols.SetUnencryptedHTTP2(true)
	s.Start()
	t.Cleanup(s.Close)
	return s.Listener.Addr().String()
}

// h2c is the route of a whole host whose replicas speak h2c.
func h2c(eps ...string) []Route {
	if eps == nil {
		eps = []string{}
	}
	return []Route{{Protocol: ProtocolH2C, Endpoints: eps}}
}

// echo is a replica with three methods. Unary takes one message and answers
// "name:message"; Server takes a count n and sends n messages, "name:0" and
// on, each one every after the one before it, if every is set, and once the
// client has asked for it on next, if next is set; Bidi answers each message
// as it comes, until the client ends. Each call ends with Grpc-Status 0. It
// records the last request's protocol and headers.
type echo struct {
	name  string
	next  chan struct{}
	every time.Duration
	mu    sync.Mutex
	last  *http.Request
}

func (e *echo) request() *http.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

func (e *echo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.last = r.Clone(context.Background())
	e.mu.Unlock()
	if r.Method != http.MethodPost {
		_, _ = io.WriteString(w, "plain "+e.name)
		return
	}
	w.Header().Set("Content-Type", "application/grpc")
	switch r.URL.Path {
	case "/echo.Echo/Unary":
		msg, err := readFrame(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = sendFrame(w, e.name+":"+msg)
	case "/echo.Echo/Server":
		msg, err := readFrame(r.Body)
		n, _ := strconv.Atoi(msg)
		if err != nil || n <= 0 {
			http.Error(w, "want a count", http.StatusBadRequest)
			return
		}
		for i := range n {
			if err := sendFrame(w, e.name+":"+strconv.Itoa(i)); err != nil {
				return
			}
			if e.every > 0 && i < n-1 {
				select {
				case <-time.After(e.every):
				case <-r.Context().Done():
					return
				}
			}
			if e.next != nil && i < n-1 {
				select {
				case <-e.next:
				case <-r.Context().Done():
					return
				}
			}
		}
	case "/echo.Echo/Bidi":
		for {
			msg, err := readFrame(r.Body)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return
			}
			if err := sendFrame(w, e.name+":"+msg); err != nil {
				return
			}
		}
	default:
		http.NotFound(w, r)
		return
	}
	grpcStatus(w, 0, "ok")
}

// grpcRequest is a call to method on host through the router.
func grpcRequest(ctx context.Context, host, method string, body io.Reader) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/echo.Echo/"+method, body)
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	return req
}

// A unary call over h2c, from a client on HTTP/2 and from one on HTTP/1.1:
// the replica gets HTTP/2, the router's forwarding headers and TE:
// trailers, and the client gets the message and the call's status in the
// trailers. A WebSocket upgrade reaches an h2c replica as a plain request,
// since HTTP/2 has no Upgrade.
func TestH2CUnaryAndTrailers(t *testing.T) {
	e := &echo{name: "up"}
	tr := newTestRouter(t, Table{"api.example.com": h2c(h2cServer(t, e.ServeHTTP))})
	for _, h1 := range []bool{false, true} {
		req := grpcRequest(context.Background(), "api.example.com", "Unary", bytes.NewReader(grpcFrame("hello")))
		req.Header.Set("X-Real-Ip", "6.6.6.6")
		resp, err := tr.client(h1).Do(req)
		if err != nil {
			t.Fatalf("h1=%v: %v", h1, err)
		}
		msg, err := readFrame(resp.Body)
		if err != nil || msg != "up:hello" {
			t.Fatalf("h1=%v: message %q, %v", h1, msg, err)
		}
		if _, err := readFrame(resp.Body); !errors.Is(err, io.EOF) {
			t.Fatalf("h1=%v: after the message: %v", h1, err)
		}
		resp.Body.Close()
		if wantMajor := map[bool]int{false: 2, true: 1}[h1]; resp.StatusCode != http.StatusOK || resp.ProtoMajor != wantMajor {
			t.Fatalf("h1=%v: status %d over HTTP/%d", h1, resp.StatusCode, resp.ProtoMajor)
		}
		if resp.Trailer.Get("Grpc-Status") != "0" || resp.Trailer.Get("Grpc-Message") != "ok" {
			t.Fatalf("h1=%v: trailers %v, header %v", h1, resp.Trailer, resp.Header)
		}
		r := e.request()
		if r.ProtoMajor != 2 || r.Header.Get("Te") != "trailers" || r.Header.Get("X-Forwarded-Proto") != "https" ||
			r.Header.Get("X-Forwarded-For") != "127.0.0.1" || r.Header.Get("X-Real-Ip") != "" || r.Host != "api.example.com:"+strconv.Itoa(tr.rt.HTTPSPort()) {
			t.Fatalf("h1=%v: the replica got HTTP/%d, host %q, headers %v", h1, r.ProtoMajor, r.Host, r.Header)
		}
	}

	resp := upgrade(t, tr.client(true), "api.example.com", "websocket")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "plain up" {
		t.Fatalf("a WebSocket upgrade on an h2c route: %d %q, want the plain reply", resp.StatusCode, b)
	}
	for _, k := range []string{"Upgrade", "Connection", "Http2-Settings"} {
		if v := e.request().Header.Values(k); len(v) > 0 {
			t.Errorf("the h2c replica got %s %q", k, v)
		}
	}
	// A request is counted when its handler returns, which can be just after
	// the client has read the reply.
	const want = `hivemind_router_requests_total{host="api.example.com",code="200"} 3`
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(tr.metrics(t), want) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m := tr.metrics(t); !strings.Contains(m, want) {
		t.Fatalf("requests:\n%s", m)
	}
}

// A server stream reaches the client message by message: the replica sends
// each one only after the client has read the one before it, so a message
// held anywhere in the router would stop the stream.
func TestH2CServerStreamFlowsAsItComes(t *testing.T) {
	e := &echo{name: "up", next: make(chan struct{})}
	tr := newTestRouter(t, Table{"api.example.com": h2c(h2cServer(t, e.ServeHTTP))})
	for _, h1 := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		resp, err := tr.client(h1).Do(grpcRequest(ctx, "api.example.com", "Server", bytes.NewReader(grpcFrame("5"))))
		if err != nil {
			cancel()
			t.Fatalf("h1=%v: %v", h1, err)
		}
		for i := range 5 {
			msg, err := readFrame(resp.Body)
			if err != nil || msg != "up:"+strconv.Itoa(i) {
				t.Fatalf("h1=%v: message %d: %q, %v", h1, i, msg, err)
			}
			if i < 4 {
				e.next <- struct{}{}
			}
		}
		if _, err := readFrame(resp.Body); !errors.Is(err, io.EOF) || resp.Trailer.Get("Grpc-Status") != "0" {
			t.Fatalf("h1=%v: end of the stream: %v, trailers %v", h1, err, resp.Trailer)
		}
		resp.Body.Close()
		cancel()
	}
}

// bidi opens a bidirectional stream to host through c, and returns the
// reply and the writer of the request body. The stream's first message goes
// with it, since the replica sends its headers with its first reply.
func bidi(t *testing.T, ctx context.Context, c *http.Client, host, first string) (*http.Response, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Do(grpcRequest(ctx, host, "Bidi", pr))
		done <- result{resp, err}
	}()
	if _, err := pw.Write(grpcFrame(first)); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	return r.resp, pw
}

// A bidirectional stream runs in lockstep: the client sends a message only
// after it has the reply to the one before, so each message must go through
// the router both ways at once, unbuffered.
func TestH2CBidirectionalStream(t *testing.T) {
	e := &echo{name: "up"}
	tr := newTestRouter(t, Table{"api.example.com": h2c(h2cServer(t, e.ServeHTTP))})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := tr.client(false)
	c.Timeout = 0
	resp, pw := bidi(t, ctx, c, "api.example.com", "m0")
	defer resp.Body.Close()
	for i := range 20 {
		if i > 0 {
			if _, err := pw.Write(grpcFrame("m" + strconv.Itoa(i))); err != nil {
				t.Fatalf("send %d: %v", i, err)
			}
		}
		if msg, err := readFrame(resp.Body); err != nil || msg != "up:m"+strconv.Itoa(i) {
			t.Fatalf("reply %d: %q, %v", i, msg, err)
		}
	}
	pw.Close()
	if _, err := readFrame(resp.Body); !errors.Is(err, io.EOF) || resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("end of the stream: %v, trailers %v", err, resp.Trailer)
	}
}

// With small limits in place of the 60 s response timeout, the 60 s write
// limit and the 5-minute request read limit, streams that keep moving for
// several times as long are not cut, either way: the response timeout
// ends once the reply's headers come, the write limit counts from each
// write, and an h2c request has no whole-request read limit. The same slow
// upload on an http route is cut by the read limit, which shows the limits
// are live. A stream that carries no data either way for the idle limit is
// ended, at both ends; and the response timeout still holds for a replica
// that sends nothing at all after the whole request.
func TestH2CLongStreamsOutliveTheTimeouts(t *testing.T) {
	const (
		limit = 200 * time.Millisecond
		idle  = time.Second
		tick  = 100 * time.Millisecond
		long  = 15 // ticks: 1.5 s, past every limit but idle
	)
	e := &echo{name: "up", every: tick}
	stalled := make(chan struct{})
	ended := make(chan struct{}, 4)
	stall := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-stalled:
		case <-r.Context().Done():
		}
	})
	quiet := h2cServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer func() { ended <- struct{}{} }()
		if msg, err := readFrame(r.Body); err == nil {
			_ = sendFrame(w, "up:"+msg)
		}
		<-r.Context().Done()
	})
	upload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		fmt.Fprintf(w, "%d %v", n, err)
	}))
	t.Cleanup(upload.Close)
	t.Cleanup(func() { close(stalled) })
	tr := newTestRouterWith(t, Table{
		"api.example.com":  h2c(h2cServer(t, e.ServeHTTP)),
		"big.example.com":  h2c(stall),
		"www.example.com":  h2c(quiet),
		"post.example.com": whole(upload.Listener.Addr().String()),
	}, testOptions{poll: time.Hour,
		config: func(c *Config) { c.WriteTimeout, c.UpgradeIdleTimeout = limit, idle },
		before: func(rt *Router) { rt.proxy.responseTimeout, rt.tlsSrv.ReadTimeout = limit, 300*time.Millisecond }})
	c := tr.client(false)
	c.Timeout = 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	resp, err := c.Do(grpcRequest(ctx, "api.example.com", "Server", bytes.NewReader(grpcFrame(strconv.Itoa(long)))))
	if err != nil {
		t.Fatal(err)
	}
	for i := range long {
		if msg, err := readFrame(resp.Body); err != nil || msg != "up:"+strconv.Itoa(i) {
			t.Fatalf("server stream, message %d after %v: %q, %v", i, time.Since(start), msg, err)
		}
	}
	if took := time.Since(start); took < time.Duration(long-1)*tick {
		t.Fatalf("the server stream took %v, want its messages %v apart", took, tick)
	}
	if _, err := readFrame(resp.Body); !errors.Is(err, io.EOF) || resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("server stream end: %v, trailers %v", err, resp.Trailer)
	}
	resp.Body.Close()

	start = time.Now()
	resp, pw := bidi(t, ctx, c, "api.example.com", "m0")
	for i := range long {
		if i > 0 {
			time.Sleep(tick)
			if _, err := pw.Write(grpcFrame("m" + strconv.Itoa(i))); err != nil {
				t.Fatalf("bidi send %d after %v: %v", i, time.Since(start), err)
			}
		}
		if msg, err := readFrame(resp.Body); err != nil || msg != "up:m"+strconv.Itoa(i) {
			t.Fatalf("bidi reply %d after %v: %q, %v", i, time.Since(start), msg, err)
		}
	}
	pw.Close()
	if _, err := readFrame(resp.Body); !errors.Is(err, io.EOF) || resp.Trailer.Get("Grpc-Status") != "0" {
		t.Fatalf("bidi end: %v, trailers %v", err, resp.Trailer)
	}
	resp.Body.Close()
	t.Logf("streams of %v each passed with limits of %v and an idle limit of %v", time.Since(start).Round(10*time.Millisecond), limit, idle)

	// The same pace of upload on an http route: cut by the read limit.
	upr, upw := io.Pipe()
	go func() {
		for range long {
			if _, err := upw.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(tick)
		}
		upw.Close()
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://post.example.com/", upr)
	if resp, err := c.Do(req); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && string(b) == strconv.Itoa(long)+" <nil>" {
			t.Fatalf("an upload of %v on an http route passed the 300 ms read limit", time.Duration(long)*tick)
		}
	}
	upr.CloseWithError(errors.New("done"))

	// Quiet both ways for the idle limit: the stream ends at both ends.
	resp, pw = bidi(t, ctx, c, "www.example.com", "m0")
	if msg, err := readFrame(resp.Body); err != nil || msg != "up:m0" {
		t.Fatalf("quiet stream's reply: %q, %v", msg, err)
	}
	last := time.Now()
	_, err = readFrame(resp.Body)
	took := time.Since(last)
	if err == nil || errors.Is(err, io.EOF) || took < idle/2 || took > 5*idle {
		t.Fatalf("a quiet stream ended %v after its last data with %v; want an error after about %v", took, err, idle)
	}
	t.Logf("a quiet stream ended %v after its last data: %v", took.Round(10*time.Millisecond), err)
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the replica's side of the quiet stream did not end")
	}
	pw.Close()
	resp.Body.Close()

	// A replica that sends nothing after the whole request: 502 after the
	// response timeout.
	start = time.Now()
	resp, err = c.Do(grpcRequest(ctx, "big.example.com", "Unary", bytes.NewReader(grpcFrame("x"))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || time.Since(start) > 5*time.Second {
		t.Fatalf("a replica that never answered: %d after %v, want 502 after about %v", resp.StatusCode, time.Since(start), limit)
	}
}

// A client that cancels a stream ends the replica's side of it; a replica
// that resets its stream gets the client an error, not a clean end without
// a status.
func TestH2CCancellationBothWays(t *testing.T) {
	gone := make(chan string, 4)
	h := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		if r.URL.Path == "/echo.Echo/Abort" {
			_ = sendFrame(w, "one")
			panic(http.ErrAbortHandler)
		}
		for i := 0; ; i++ {
			if err := sendFrame(w, strconv.Itoa(i)); err != nil {
				break
			}
			select {
			case <-r.Context().Done():
				gone <- r.URL.Path
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		<-r.Context().Done()
		gone <- r.URL.Path
	}
	for _, h1 := range []bool{false, true} {
		for _, method := range []string{"Server", "Bidi"} {
			// A router and replica of its own for each case: a cancelled
			// stream may end the HTTP/2 connection it was on, and the next
			// case must not start on that connection.
			tr := newTestRouter(t, Table{"api.example.com": h2c(h2cServer(t, h))})
			c := tr.client(h1)
			c.Timeout = 0
			ctx, cancel := context.WithCancel(context.Background())
			var body io.Reader = bytes.NewReader(grpcFrame("1"))
			var pw *io.PipeWriter
			if method == "Bidi" && !h1 {
				var pr *io.PipeReader
				pr, pw = io.Pipe()
				body = pr
			}
			resp, err := c.Do(grpcRequest(ctx, "api.example.com", method, body))
			if err != nil {
				cancel()
				t.Fatalf("h1=%v %s: %v", h1, method, err)
			}
			for range 2 {
				if _, err := readFrame(resp.Body); err != nil {
					t.Fatalf("h1=%v %s: %v", h1, method, err)
				}
			}
			// Go's client sends no reset for a cancel while it waits for more
			// of the body to send, so the reply is closed too, as a client
			// that gives up on a stream does.
			cancel()
			resp.Body.Close()
			select {
			case p := <-gone:
				if p != "/echo.Echo/"+method {
					t.Fatalf("h1=%v %s: the replica ended %s", h1, method, p)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("h1=%v %s: the replica's stream did not end when the client cancelled", h1, method)
			}
			if pw != nil {
				pw.Close()
			}
		}

		tr := newTestRouter(t, Table{"api.example.com": h2c(h2cServer(t, h))})
		c := tr.client(h1)
		c.Timeout = 0
		resp, err := c.Do(grpcRequest(context.Background(), "api.example.com", "Abort", bytes.NewReader(grpcFrame("x"))))
		if err != nil {
			t.Fatalf("h1=%v: %v", h1, err)
		}
		if msg, err := readFrame(resp.Body); err != nil || msg != "one" {
			t.Fatalf("h1=%v: before the reset: %q, %v", h1, msg, err)
		}
		_, err = readFrame(resp.Body)
		resp.Body.Close()
		if err == nil || errors.Is(err, io.EOF) || resp.Trailer.Get("Grpc-Status") != "" {
			t.Fatalf("h1=%v: after the replica reset its stream: %v, trailers %v; want an error", h1, err, resp.Trailer)
		}
	}
}

// A dead h2c endpoint is skipped as an http one is: the first dial fails
// and is counted, the request goes to the next replica, and the endpoint is
// marked down. One address can be an endpoint of an http route and of an
// h2c route at once, each with its own state; an h2c-only replica behind an
// http route gets 502, since it speaks no HTTP/1.1. A route with a protocol
// the router does not have gets 503.
func TestH2CEndpointsAndBackoff(t *testing.T) {
	e := &echo{name: "up"}
	live, dead := h2cServer(t, e.ServeHTTP), deadAddr(t)
	tr := newTestRouter(t, Table{
		"api.example.com":  h2c(dead, live),
		"www.example.com":  whole(live),
		"post.example.com": whole(dead),
		"big.example.com":  {{Protocol: "h3", Endpoints: []string{live}}},
	})
	c := tr.client(false)
	for i := range 6 {
		resp, err := c.Do(grpcRequest(context.Background(), "api.example.com", "Unary", bytes.NewReader(grpcFrame("x"))))
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := readFrame(resp.Body)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || msg != "up:x" || resp.Trailer.Get("Grpc-Status") != "0" {
			t.Fatalf("call %d: %d %q %v", i, resp.StatusCode, msg, resp.Trailer)
		}
	}
	if m := tr.metrics(t); !strings.Contains(m, `hivemind_router_upstream_errors_total{host="api.example.com",kind="dial"} 1`) {
		t.Fatalf("dial errors:\n%s", m)
	}
	p := tr.rt.proxy
	p.mu.RLock()
	dh2c, dhttp, lh2c, lhttp := p.endpoints[epKey{dead, true}], p.endpoints[epKey{dead, false}], p.endpoints[epKey{live, true}], p.endpoints[epKey{live, false}]
	n := len(p.endpoints)
	p.mu.RUnlock()
	if n != 4 || dh2c == nil || dhttp == nil || lh2c == nil || lhttp == nil || !dh2c.h2c || dhttp.h2c {
		t.Fatalf("%d endpoints: %v %v %v %v", n, dh2c, dhttp, lh2c, lhttp)
	}
	if dh2c.fails.Load() != 1 || dh2c.downUntil.Load() <= time.Now().UnixNano() || dhttp.fails.Load() != 0 {
		t.Fatalf("the dead h2c endpoint: %d failures, down until %v; the http one: %d failures",
			dh2c.fails.Load(), time.Unix(0, dh2c.downUntil.Load()), dhttp.fails.Load())
	}

	req, _ := http.NewRequest(http.MethodGet, "https://www.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an h2c-only replica behind an http route: %d %q, want 502", resp.StatusCode, body)
	}
	req, _ = http.NewRequest(http.MethodGet, "https://big.example.com/", nil)
	if resp, _, body := get(t, c, req); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a route with an unknown protocol: %d %q, want 503", resp.StatusCode, body)
	}
	// The route goes over to http: a new endpoint, and the h2c one leaves.
	tr.rt.SetTable(Table{"api.example.com": whole(live)})
	p.mu.RLock()
	_, stays := p.endpoints[epKey{live, false}]
	_, h2cStays := p.endpoints[epKey{live, true}]
	p.mu.RUnlock()
	if !stays || h2cStays {
		t.Fatalf("after the change to http: http endpoint %v, h2c endpoint %v", stays, h2cStays)
	}
}
