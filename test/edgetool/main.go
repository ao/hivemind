// Command edgetool is the helper of scripts/edge-suite.sh. It runs inside a
// test node and is not part of hivemind.
//
//	edgetool registry -listen 127.0.0.1:5001 -mode 429 -log FILE
//	edgetool write -acks FILE [-workers 4]
//	edgetool verify -acks FILE
//	edgetool storecheck -db /var/lib/hivemind/hivemind.db
//
// registry is a stand-in for an image registry that fails in one way: mode
// 429 answers every request with 429 Too Many Requests, slow holds every
// request for -delay before it answers 503, and notfound answers 404 with
// MANIFEST_UNKNOWN. It appends one line per request to -log: the Unix time
// in milliseconds, the method and the path. It listens on loopback only.
//
// write sends API writes as fast as the server answers them, from -workers
// goroutines that each own their keys, so the writes to one key are in
// order and at most one is in flight. Before each write it appends a "sent"
// line to -acks, and after a 2xx answer an "ack" line. It stops at the first
// write that gets no answer, because the server went away, and exits 0.
//
// verify reads -acks and checks the server's state: for each key, the value
// is that of the last acknowledged write, or of a write sent after it that
// was never answered, which the server may or may not have committed.
//
// storecheck opens the store, which recovers its WAL, and runs the store's
// invariant check (store.CheckInvariants), SQLite's integrity_check among
// them. Run it while serve is stopped.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: edgetool registry|write|verify|storecheck [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "registry":
		err = registry(os.Args[2:])
	case "write":
		err = write(os.Args[2:])
	case "verify":
		err = verify(os.Args[2:])
	case "storecheck":
		err = storecheck(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "edgetool:", err)
		os.Exit(1)
	}
}

func registry(args []string) error {
	fs := flag.NewFlagSet("registry", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:5001", "loopback address to listen on")
	mode := fs.String("mode", "429", "429, slow or notfound")
	delay := fs.Duration("delay", 15*time.Minute, "how long slow holds each request")
	logPath := fs.String("log", "", "file to append one line per request to")
	_ = fs.Parse(args)
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-listen %s: loopback addresses only", *listen)
	}
	var mu sync.Mutex
	var logf io.Writer = io.Discard
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		logf = f
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fmt.Fprintf(logf, "%d %s %s\n", time.Now().UnixMilli(), r.Method, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.Header().Set("Content-Type", "application/json")
		switch *mode {
		case "429":
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintln(w, `{"errors":[{"code":"TOOMANYREQUESTS","message":"edgetool: rate limit"}]}`)
		case "slow":
			select {
			case <-r.Context().Done():
				return
			case <-time.After(*delay):
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"errors":[{"code":"UNAVAILABLE","message":"edgetool: slow"}]}`)
		case "notfound":
			if r.URL.Path == "/v2/" {
				fmt.Fprintln(w, `{}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(w, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
		default:
			http.Error(w, "bad mode", http.StatusInternalServerError)
		}
	})
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// entry is one line of the acks file.
type entry struct {
	Kind  string `json:"kind"` // sent or ack
	Key   string `json:"key"`  // "app/<name>" or "registry/<host>"
	Op    string `json:"op"`   // scale, create or login
	Value string `json:"value"`
	Seq   int64  `json:"seq"`
}

type api struct {
	base  string
	token string
	http  *http.Client
}

func newAPI() (*api, error) {
	tok, err := os.ReadFile("/var/lib/hivemind/admin.token")
	if err != nil {
		return nil, err
	}
	return &api{base: "http://127.0.0.1:4483", token: strings.TrimSpace(string(tok)),
		http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// do sends one request. answered is false when no HTTP response came back.
func (a *api) do(method, path string, body any) (code int, out []byte, answered bool, err error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, false, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, a.base+path, rd)
	if err != nil {
		return 0, nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, nil, false, err
	}
	defer resp.Body.Close()
	out, err = io.ReadAll(resp.Body)
	if err != nil {
		// The status line came, but the body broke off: the write may or
		// may not be committed, so it counts as unanswered.
		return resp.StatusCode, nil, false, err
	}
	return resp.StatusCode, out, true, nil
}

func write(args []string) error {
	fs := flag.NewFlagSet("write", flag.ExitOnError)
	acksPath := fs.String("acks", "", "file to append sent and ack lines to")
	workers := fs.Int("workers", 4, "concurrent writers, each with its own keys")
	apps := fs.Int("apps", 2, "apps each worker scales")
	_ = fs.Parse(args)
	a, err := newAPI()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(*acksPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	var mu sync.Mutex
	record := func(e entry) {
		b, _ := json.Marshal(e)
		mu.Lock()
		defer mu.Unlock()
		_, _ = f.Write(append(b, '\n'))
	}
	// The apps the workers scale are made once, with one replica, before
	// the first round.
	for w := range *workers {
		for j := range *apps {
			name := fmt.Sprintf("a%d-%d", w, j)
			code, _, answered, err := a.do(http.MethodGet, "/v1/apps/"+name, nil)
			if !answered {
				return fmt.Errorf("GET %s: %v", name, err)
			}
			if code == http.StatusNotFound {
				code, out, _, err := a.do(http.MethodPut, "/v1/apps/"+name,
					map[string]any{"image": "busybox:stable", "replicas": 1, "command": []string{"sleep", "86400"}})
				if err != nil || code/100 != 2 {
					return fmt.Errorf("creating %s: %d %s %v", name, code, out, err)
				}
			}
		}
	}
	// Sequence numbers go on from earlier rounds, so values never repeat.
	base := time.Now().UnixMicro()
	var stop sync.Once
	done := make(chan struct{})
	var wg sync.WaitGroup
	counts := make([]int, *workers)
	for w := range *workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int64(0); ; i++ {
				select {
				case <-done:
					return
				default:
				}
				seq := base + i*int64(*workers) + int64(w)
				var e entry
				var method, path string
				var body any
				switch {
				case i%200 == 199:
					name := fmt.Sprintf("k%d", seq%1_000_000_000)
					e = entry{Key: "app/" + name, Op: "create", Value: "1", Seq: seq}
					method, path = http.MethodPut, "/v1/apps/"+name
					body = map[string]any{"image": "busybox:stable", "replicas": 1, "command": []string{"sleep", "86400"}}
				case i%2 == 0:
					app := fmt.Sprintf("a%d-%d", w, (i/2)%int64(*apps))
					n := 1 + seq%3
					e = entry{Key: "app/" + app, Op: "scale", Value: fmt.Sprint(n), Seq: seq}
					method, path = http.MethodPost, "/v1/apps/"+app+"/scale"
					body = map[string]int64{"replicas": n}
				default:
					host := fmt.Sprintf("reg-%d.edge.test", w)
					user := fmt.Sprintf("u%d", seq)
					e = entry{Key: "registry/" + host, Op: "login", Value: user, Seq: seq}
					method, path = http.MethodPut, "/v1/registries/"+host
					body = map[string]string{"username": user, "password": "pw"}
				}
				e.Kind = "sent"
				record(e)
				code, out, answered, err := a.do(method, path, body)
				if !answered {
					fmt.Fprintf(os.Stderr, "edgetool: worker %d: %s %s: no answer (%v); stopping\n", w, method, path, err)
					stop.Do(func() { close(done) })
					return
				}
				if code/100 != 2 {
					fmt.Fprintf(os.Stderr, "edgetool: worker %d: %s %s: %d %s\n", w, method, path, code, bytes.TrimSpace(out))
					if code >= 500 {
						// A 5xx may come after the change was committed.
						stop.Do(func() { close(done) })
						return
					}
					e.Kind = "refused"
					record(e)
					continue
				}
				e.Kind = "ack"
				record(e)
				counts[w]++
			}
		}()
	}
	wg.Wait()
	total := 0
	for _, n := range counts {
		total += n
	}
	fmt.Printf("edgetool: %d writes acknowledged\n", total)
	return nil
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	acksPath := fs.String("acks", "", "the acks file written by write")
	_ = fs.Parse(args)
	f, err := os.Open(*acksPath)
	if err != nil {
		return err
	}
	defer f.Close()
	type keyState struct {
		op      string
		acked   *entry
		pending []entry // sent after the last ack, never answered
	}
	keys := map[string]*keyState{}
	sc := bufio.NewScanner(f)
	nacks := 0
	for sc.Scan() {
		var e entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			// The writer is never killed, so a torn line is a bug.
			return fmt.Errorf("acks file: %v: %q", err, sc.Text())
		}
		k := keys[e.Key]
		if k == nil {
			k = &keyState{op: e.Op}
			keys[e.Key] = k
		}
		switch e.Kind {
		case "sent":
			k.pending = append(k.pending, e)
		case "refused":
			k.pending = slices.DeleteFunc(k.pending, func(p entry) bool { return p.Seq == e.Seq })
		case "ack":
			nacks++
			// The writes to one key are in order, so this one overwrites
			// any earlier write that was never answered.
			k.pending = slices.DeleteFunc(k.pending, func(p entry) bool { return p.Seq <= e.Seq })
			ec := e
			k.acked = &ec
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	a, err := newAPI()
	if err != nil {
		return err
	}
	code, out, _, err := a.do(http.MethodGet, "/v1/apps", nil)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("GET /v1/apps: %d %v", code, err)
	}
	var appList []struct {
		Name     string `json:"name"`
		Replicas int    `json:"replicas"`
	}
	if err := json.Unmarshal(out, &appList); err != nil {
		return err
	}
	appReplicas := map[string]int{}
	for _, x := range appList {
		appReplicas[x.Name] = x.Replicas
	}
	code, out, _, err = a.do(http.MethodGet, "/v1/registries", nil)
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("GET /v1/registries: %d %v", code, err)
	}
	var regs struct {
		Registries []struct {
			Host     string `json:"host"`
			Username string `json:"username"`
		} `json:"registries"`
	}
	if err := json.Unmarshal(out, &regs); err != nil {
		return err
	}
	regUser := map[string]string{}
	for _, r := range regs.Registries {
		regUser[r.Host] = r.Username
	}

	var problems []string
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		k := keys[name]
		allowed := map[string]bool{}
		if k.acked != nil {
			allowed[k.acked.Value] = true
		} else {
			// Never acknowledged: the key keeps its value from before.
			switch k.op {
			case "scale":
				allowed["1"] = true // the setup deploys each scaled app with one replica
			default:
				allowed[""] = true // absent
			}
		}
		for _, p := range k.pending {
			allowed[p.Value] = true
		}
		var got string
		kind, id, _ := strings.Cut(name, "/")
		switch kind {
		case "app":
			if n, ok := appReplicas[id]; ok {
				got = fmt.Sprint(n)
			}
		case "registry":
			got = regUser[id]
		}
		if !allowed[got] {
			want := make([]string, 0, len(allowed))
			for v := range allowed {
				want = append(want, fmt.Sprintf("%q", v))
			}
			sort.Strings(want)
			problems = append(problems, fmt.Sprintf("%s is %q, want one of %s", name, got, strings.Join(want, ", ")))
		}
	}
	fmt.Printf("edgetool: verified %d keys against %d acknowledged writes\n", len(keys), nacks)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("edgetool: LOST:", p)
		}
		return fmt.Errorf("%d keys do not hold an acknowledged value", len(problems))
	}
	return nil
}

func storecheck(args []string) error {
	fs := flag.NewFlagSet("storecheck", flag.ExitOnError)
	path := fs.String("db", "/var/lib/hivemind/hivemind.db", "the store file")
	_ = fs.Parse(args)
	if _, err := os.Stat(*path); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	st, err := store.Open(ctx, *path)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.CheckInvariants(ctx); err != nil {
		return fmt.Errorf("invariants: %w", err)
	}
	fmt.Println("edgetool: store ok: integrity_check ok, invariants hold")
	return nil
}
