package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
)

func TestServerNodeAddress(t *testing.T) {
	ctx := context.Background()
	for in, want := range map[string]string{"": "127.0.0.1", "10.0.0.5": "10.0.0.5", "::ffff:10.0.0.5": "10.0.0.5",
		"fd00::5": "fd00::5", "localhost": "127.0.0.1"} {
		got, err := serverNodeAddress(ctx, in)
		if err != nil || got != want {
			t.Errorf("serverNodeAddress(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := serverNodeAddress(ctx, "no-such-host.invalid"); err == nil {
		t.Error("a name that does not resolve was accepted")
	}
	if err := checkBoundAddress("10.0.0.5:4484", "10.0.0.5"); err != nil {
		t.Error(err)
	}
	if err := checkBoundAddress("[::1]:4484", "127.0.0.1"); err == nil || !strings.Contains(err.Error(), "give --advertise as an IP address") {
		t.Errorf("a bound address unlike the node's: %v", err)
	}
}

// fakeRegistrar refuses a registration with an address, as a 2.1 server's
// decoder refuses a field it does not know, when old is set.
type fakeRegistrar struct {
	old   bool
	calls []nodeapi.Register
}

func (f *fakeRegistrar) Register(_ context.Context, _ string, g nodeapi.Register) error {
	f.calls = append(f.calls, g)
	if f.old && g.Address != "" {
		return &nodeapi.RefusedError{Code: http.StatusBadRequest, Status: "400 Bad Request", Message: "invalid request body"}
	}
	return nil
}

func TestRegisterFallsBackForAnOlderServer(t *testing.T) {
	ctx := context.Background()
	logs := &syncLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	reg := nodeapi.Register{Name: "w", CPU: 2, Memory: 1 << 30, Address: "10.0.0.6"}

	newer := &fakeRegistrar{}
	if err := registerWith(ctx, newer, "node-w", reg, log); err != nil || len(newer.calls) != 1 || newer.calls[0].Address != "10.0.0.6" {
		t.Fatalf("a 2.2 server: %v, %+v", err, newer.calls)
	}
	older := &fakeRegistrar{old: true}
	if err := registerWith(ctx, older, "node-w", reg, log); err != nil || len(older.calls) != 2 || older.calls[1].Address != "" {
		t.Fatalf("an older server: %v, %+v", err, older.calls)
	}
	if !strings.Contains(logs.String(), "takes no routed replicas") {
		t.Errorf("the fallback was not logged: %s", logs.String())
	}
	// Without an address there is nothing to retry.
	noAddr := &fakeRegistrar{old: true}
	reg.Address = ""
	if err := registerWith(ctx, noAddr, "node-w", reg, log); err != nil || len(noAddr.calls) != 1 {
		t.Fatalf("no address: %v, %+v", err, noAddr.calls)
	}
}

// serve with --route-addr runs the router on that address beside the API:
// it knows the routed hosts from the store, redirects them to https, and
// answers 404 for other hosts. No ACME CA is reachable here, so there is no
// certificate, and the test does not use the https port.
func TestServeRunsTheRouter(t *testing.T) {
	home := isolate(t)
	data := filepath.Join(home, "data")
	if _, _, err := run(t, "", "init", "--data-dir", data); err != nil {
		t.Fatal(err)
	}
	token, err := auth.ReadAdminToken(data)
	if err != nil {
		t.Fatal(err)
	}
	apiPort, httpPort, httpsPort := unusedPort(t), unusedPort(t), unusedPort(t)
	env := map[string]string{
		"HIVEMIND_DATA_DIR":           data,
		"HIVEMIND_API_ADDR":           "127.0.0.1:" + strconv.Itoa(apiPort),
		"HIVEMIND_RECONCILE_INTERVAL": "50ms",
		"HIVEMIND_MIN_READY":          "0s",
		"HIVEMIND_ROUTE_ADDR":         "127.0.0.1",
		"HIVEMIND_ROUTE_HTTP_PORT":    strconv.Itoa(httpPort),
		"HIVEMIND_ROUTE_HTTPS_PORT":   strconv.Itoa(httpsPort),
		"HIVEMIND_ACME_EMAIL":         "ops@example.com",
		// .invalid never resolves (RFC 6761), so no request leaves the host.
		"HIVEMIND_ACME_CA": "https://acme.invalid/directory",
	}
	cfg, err := config.Load(nil, config.Options{Lookup: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncLog{}
	log, err := newLogger(cfg, logs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, log, newMemRuntime()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("serve did not stop")
		}
	})

	api := "http://127.0.0.1:" + strconv.Itoa(apiPort)
	waitUntil(t, "healthz", func() bool {
		resp, err := http.Get(api + "/healthz")
		if err == nil {
			resp.Body.Close()
		}
		return err == nil && resp.StatusCode == 200
	})
	do := func(method, url, host, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if host != "" {
			req.Host = host
		}
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if resp, body := do(http.MethodPut, api+"/v1/apps/web", "",
		`{"image":"nginx","replicas":2,"routes":[{"host":"www.example.com","port":80}]}`); resp.StatusCode/100 != 2 {
		t.Fatalf("put: %d %s", resp.StatusCode, body)
	}
	var routes struct {
		Routes []struct {
			Host      string   `json:"host"`
			Endpoints []string `json:"endpoints"`
		} `json:"routes"`
	}
	_, body := do(http.MethodGet, api+"/v1/routes", "", "")
	if err := json.Unmarshal([]byte(body), &routes); err != nil || len(routes.Routes) != 1 || routes.Routes[0].Host != "www.example.com" {
		t.Fatalf("routes: %s", body)
	}

	router := "http://127.0.0.1:" + strconv.Itoa(httpPort)
	want := fmt.Sprintf("https://www.example.com:%d/x?y=1", httpsPort)
	waitUntil(t, "redirect", func() bool {
		resp, _ := do(http.MethodGet, router+"/x?y=1", "www.example.com", "")
		return resp.StatusCode == http.StatusPermanentRedirect && resp.Header.Get("Location") == want
	})
	if resp, body := do(http.MethodGet, router+"/", "other.example.com", ""); resp.StatusCode != http.StatusNotFound || strings.Contains(body, "www") {
		t.Fatalf("unknown host: %d %q", resp.StatusCode, body)
	}
	_, metrics := do(http.MethodGet, api+"/metrics", "", "")
	for _, want := range []string{`hivemind_router_requests_total{host="www.example.com",code="308"}`,
		`hivemind_router_requests_total{host="unknown",code="404"}`} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics have no %s", want)
		}
	}
	if s := logs.String(); !strings.Contains(s, "router listening") {
		t.Errorf("no router line in the log:\n%s", s)
	}
}
