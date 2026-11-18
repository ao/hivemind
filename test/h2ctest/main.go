// Command h2ctest is the gRPC server and client of the h2c routing test in
// scripts/integration-routing.sh. It is not part of hivemind.
//
//	h2ctest serve [-addr 127.0.0.1:50051] [-name NAME]
//	h2ctest call -url URL -connect IP:PORT -ca FILE [-h1] unary|server|bidi
//	h2ctest loop -url URL -ca FILE -out FILE IP:PORT...
//
// It speaks gRPC's wire format over HTTP/2 with Go's standard library
// alone: a call is a POST to /h2ctest.Echo/<Method> with content-type
// application/grpc and TE: trailers, each message is a flag byte, a 4-byte
// big-endian length and the message, and the call's status is the
// grpc-status trailer. The messages are text, not protobuf: the router
// routes HTTP/2 frames and never reads a message, so protobuf would test
// nothing more of it, and a server built here needs no image from outside.
//
// serve speaks HTTP/2 without TLS (h2c) and no HTTP/1.1, as a gRPC server
// does, on -addr, which is loopback unless given. Unary answers its message
// as "NAME/V@HOST:message", where V is $V and HOST the host name; Server
// takes "N MS" and sends N such messages, MS milliseconds apart; Bidi
// answers each message as it comes, until the client ends. On SIGTERM it
// sends GOAWAY and lets open calls finish, for up to 8 s.
//
// call makes one call to URL, a host and port that the router serves, over
// TLS to -connect with the CA in -ca, on HTTP/2 or with -h1 on HTTP/1.1,
// and checks it: the messages, from one replica, the protocol, and
// grpc-status 0. It prints the replica's name and exits 1 on a failure.
//
// loop makes a unary call, a server stream and a bidirectional stream
// through each IP:PORT in turn until the file -out.stop exists, and appends
// a line for each to -out: "KIND ok NAME" or "KIND fail ERROR".
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: h2ctest serve|call|loop [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "call":
		err = callCmd(os.Args[2:])
	case "loop":
		err = loop(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatalf("h2ctest %s: %v", os.Args[1], err)
	}
}

// frame is one message in gRPC's framing: not compressed, its length, the
// message.
func frame(msg string) []byte {
	b := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(b[1:5], uint32(len(msg)))
	copy(b[5:], msg)
	return b
}

// readFrame reads one message. io.EOF is a clean end between messages.
func readFrame(r io.Reader) (string, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if h[0] != 0 || n > 1<<16 {
		return "", fmt.Errorf("a message with flag %d and length %d", h[0], n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:50051", "address to listen on")
	name := fs.String("name", "h2ctest", "name in replies")
	_ = fs.Parse(args)
	host, _ := os.Hostname()
	id := *name + "/" + os.Getenv("V") + "@" + host

	send := func(w http.ResponseWriter, msg string) error {
		if _, err := w.Write(frame(id + ":" + msg)); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	done := func(w http.ResponseWriter) {
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "0")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "")
	}
	fail := func(w http.ResponseWriter, err error) {
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "3") // INVALID_ARGUMENT
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", err.Error())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /h2ctest.Echo/Unary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		msg, err := readFrame(r.Body)
		if err != nil {
			fail(w, err)
			return
		}
		if send(w, msg) == nil {
			done(w)
		}
	})
	mux.HandleFunc("POST /h2ctest.Echo/Server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		msg, err := readFrame(r.Body)
		var n, ms int
		if err == nil {
			_, err = fmt.Sscanf(msg, "%d %d", &n, &ms)
		}
		if err != nil || n < 1 || n > 1000 || ms < 0 || ms > 60000 {
			fail(w, fmt.Errorf("want \"N MS\": %q, %v", msg, err))
			return
		}
		for i := range n {
			if i > 0 {
				select {
				case <-time.After(time.Duration(ms) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			if send(w, strconv.Itoa(i)) != nil {
				return
			}
		}
		done(w)
	})
	mux.HandleFunc("POST /h2ctest.Echo/Bidi", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc")
		for {
			msg, err := readFrame(r.Body)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return
			}
			if send(w, msg) != nil {
				return
			}
		}
		done(w)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Minute}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetUnencryptedHTTP2(true)
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Printf("h2ctest %s listening on %s, h2c only", id, ln.Addr())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	select {
	case err := <-errc:
		return err
	case s := <-sig:
		log.Printf("h2ctest %s: %v; finishing open calls", id, s)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// client returns an HTTP client that reaches every host at connect over TLS
// with the roots in ca, speaking HTTP/2 only, or HTTP/1.1 only with h1.
func client(ca, connect string, h1 bool) (*http.Client, error) {
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate in %s", ca)
	}
	tr := &http.Transport{
		Proxy:           nil,
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, connect)
		},
		ForceAttemptHTTP2: true,
	}
	tr.Protocols = new(http.Protocols)
	if h1 {
		tr.Protocols.SetHTTP1(true)
	} else {
		tr.Protocols.SetHTTP2(true)
	}
	return &http.Client{Transport: tr}, nil
}

// call makes one call of kind and checks it. It returns the replica's name.
func call(ctx context.Context, c *http.Client, url, kind string, h1 bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var body io.Reader
	var pw *io.PipeWriter
	var want []string
	switch kind {
	case "unary":
		body, want = bytes.NewReader(frame("ping")), []string{"ping"}
	case "server":
		body, want = bytes.NewReader(frame("5 50")), []string{"0", "1", "2", "3", "4"}
	case "bidi":
		var pr *io.PipeReader
		pr, pw = io.Pipe()
		defer pw.Close()
		body, want = pr, []string{"m0", "m1", "m2"}
	default:
		return "", fmt.Errorf("unknown kind %q", kind)
	}
	method := map[string]string{"unary": "Unary", "server": "Server", "bidi": "Bidi"}[kind]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(url, "/")+"/h2ctest.Echo/"+method, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	type result struct {
		resp *http.Response
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := c.Do(req)
		got <- result{resp, err}
	}()
	if pw != nil {
		// The replica sends its headers with its first reply.
		if _, err := pw.Write(frame(want[0])); err != nil {
			return "", err
		}
	}
	r := <-got
	if r.err != nil {
		return "", r.err
	}
	resp := r.resp
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("status %d: %q", resp.StatusCode, b)
	}
	if wantMajor := map[bool]int{false: 2, true: 1}[h1]; resp.ProtoMajor != wantMajor {
		return "", fmt.Errorf("HTTP/%d from the router, want HTTP/%d", resp.ProtoMajor, wantMajor)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/grpc" {
		return "", fmt.Errorf("content-type %q", ct)
	}
	name := ""
	for i, w := range want {
		if pw != nil && i > 0 {
			if _, err := pw.Write(frame(w)); err != nil {
				return "", fmt.Errorf("send %d: %w", i, err)
			}
		}
		msg, err := readFrame(resp.Body)
		if err != nil {
			return "", fmt.Errorf("message %d: %w", i, err)
		}
		from, text, ok := strings.Cut(msg, ":")
		if !ok || text != w || (name != "" && from != name) {
			return "", fmt.Errorf("message %d: %q, want %q from %s", i, msg, w, name)
		}
		name = from
	}
	if pw != nil {
		pw.Close()
	}
	if _, err := readFrame(resp.Body); !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("after the last message: %v", err)
	}
	if s := resp.Trailer.Get("Grpc-Status"); s != "0" {
		return "", fmt.Errorf("grpc-status %q, message %q", s, resp.Trailer.Get("Grpc-Message"))
	}
	return name, nil
}

func callCmd(args []string) error {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	url := fs.String("url", "", "https://HOST:PORT the router serves")
	connect := fs.String("connect", "", "IP:PORT of the router")
	ca := fs.String("ca", "", "file of the roots to trust")
	h1 := fs.Bool("h1", false, "speak HTTP/1.1 to the router")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *url == "" || *connect == "" || *ca == "" {
		return errors.New("want -url, -connect, -ca and one of unary, server, bidi")
	}
	c, err := client(*ca, *connect, *h1)
	if err != nil {
		return err
	}
	name, err := call(context.Background(), c, *url, fs.Arg(0), *h1)
	if err != nil {
		return err
	}
	fmt.Println(name)
	return nil
}

func loop(args []string) error {
	fs := flag.NewFlagSet("loop", flag.ExitOnError)
	url := fs.String("url", "", "https://HOST:PORT the router serves")
	ca := fs.String("ca", "", "file of the roots to trust")
	out := fs.String("out", "", "file to append a line per call to")
	_ = fs.Parse(args)
	if fs.NArg() == 0 || *url == "" || *ca == "" || *out == "" {
		return errors.New("want -url, -ca, -out and one or more IP:PORT")
	}
	var clients []*http.Client
	for _, a := range fs.Args() {
		c, err := client(*ca, a, false)
		if err != nil {
			return err
		}
		clients = append(clients, c)
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		for _, c := range clients {
			if _, err := os.Stat(*out + ".stop"); err == nil {
				return nil
			}
			for _, kind := range []string{"unary", "server", "bidi"} {
				name, err := call(context.Background(), c, *url, kind, false)
				line := kind + " ok " + name
				if err != nil {
					line = kind + " fail " + strings.ReplaceAll(err.Error(), "\n", " ")
				}
				if _, err := fmt.Fprintln(f, line); err != nil {
					return err
				}
			}
		}
	}
}
