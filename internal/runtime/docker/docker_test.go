package docker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// exchange is one request the fake daemon received, with the API version
// prefix removed from Path. Path is escaped, as sent on the wire.
type exchange struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

type handler func(w http.ResponseWriter, ex exchange)

// fakeDaemon answers requests with h and records them. It also rejects any
// request that is not sent under the API version prefix.
type fakeDaemon struct {
	mu   sync.Mutex
	reqs []exchange
}

func (f *fakeDaemon) requests() []exchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exchange(nil), f.reqs...)
}

// newFake starts a fake daemon on 127.0.0.1 and returns a client for it.
func newFake(t *testing.T, h handler) (*Client, *fakeDaemon) {
	t.Helper()
	fd := &fakeDaemon{}
	prefix := "/v" + APIVersion
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p := r.URL.EscapedPath()
		if !strings.HasPrefix(p, prefix+"/") {
			t.Errorf("request %s %s is not under %s", r.Method, p, prefix)
			http.NotFound(w, r)
			return
		}
		ex := exchange{Method: r.Method, Path: strings.TrimPrefix(p, prefix), Query: r.URL.Query(), Body: body}
		fd.mu.Lock()
		fd.reqs = append(fd.reqs, ex)
		fd.mu.Unlock()
		h(w, ex)
	}))
	t.Cleanup(srv.Close)
	c, err := New("tcp://" + srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return c, fd
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func requireIs(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want errors.Is(err, %v)", err, want)
	}
}

func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requireNotIs(t *testing.T, err, not error) {
	t.Helper()
	if errors.Is(err, not) {
		t.Fatalf("err = %v, which must not match %v", err, not)
	}
}

func requireRuntimeError(t *testing.T, err error) *runtime.Error {
	t.Helper()
	var re *runtime.Error
	if !errors.As(err, &re) {
		t.Fatalf("err = %T %v, want *runtime.Error", err, err)
	}
	return re
}

func TestNewHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	tests := []struct {
		name    string
		host    string
		env     string
		wantURL string // base URL, empty when an error is expected
	}{
		{name: "default socket", host: "", wantURL: "http://docker"},
		{name: "DOCKER_HOST unix", host: "", env: "unix:///run/x.sock", wantURL: "http://docker"},
		{name: "explicit unix", host: "unix:///var/run/docker.sock", wantURL: "http://docker"},
		{name: "unix relative path", host: "unix://relative.sock"},
		{name: "loopback tcp", host: "tcp://127.0.0.1:2375", wantURL: "http://127.0.0.1:2375"},
		{name: "localhost tcp", host: "tcp://localhost:2375", wantURL: "http://localhost:2375"},
		{name: "ipv6 loopback tcp", host: "tcp://[::1]:2375", wantURL: "http://[::1]:2375"},
		{name: "remote tcp refused", host: "tcp://10.0.0.5:2375"},
		{name: "hostname tcp refused", host: "tcp://daemon.example.com:2375"},
		{name: "tcp without port", host: "tcp://127.0.0.1"},
		{name: "ssh refused", host: "ssh://user@host"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("DOCKER_HOST", tc.env)
			}
			c, err := New(tc.host)
			if tc.wantURL == "" {
				if err == nil {
					t.Fatalf("New(%q) succeeded, want an error", tc.host)
				}
				return
			}
			requireNoErr(t, err)
			if c.base != tc.wantURL {
				t.Fatalf("base = %q, want %q", c.base, tc.wantURL)
			}
		})
	}
}

func TestPullNormalisesAndDrainsStream(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name      string
		ref       string
		wantQuery url.Values
	}{
		{
			name:      "short name",
			ref:       "nginx",
			wantQuery: url.Values{"fromImage": {"docker.io/library/nginx"}, "tag": {"latest"}},
		},
		{
			name:      "registry and tag",
			ref:       "ghcr.io/org/app:1.2",
			wantQuery: url.Values{"fromImage": {"ghcr.io/org/app"}, "tag": {"1.2"}},
		},
		{
			name:      "digest",
			ref:       "nginx@" + digest,
			wantQuery: url.Values{"fromImage": {"docker.io/library/nginx@" + digest}, "tag": {""}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
				w.Header().Set("Content-Type", "application/json")
				// Progress lines and a success status, drained with no error.
				_, _ = io.WriteString(w, `{"status":"Pulling from library/nginx"}`+"\n"+
					`{"status":"Downloading","progressDetail":{"current":1,"total":2}}`+"\n"+
					`{"status":"Status: Downloaded newer image"}`+"\n")
			})
			requireNoErr(t, c.Pull(context.Background(), tc.ref))
			reqs := fd.requests()
			if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/images/create" {
				t.Fatalf("requests = %+v, want one POST /images/create", reqs)
			}
			for k, want := range tc.wantQuery {
				if got := reqs[0].Query.Get(k); got != want[0] {
					t.Errorf("query %s = %q, want %q", k, got, want[0])
				}
			}
		})
	}
}

func TestPullErrorsInStream(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantNF   bool
		wantText string
	}{
		{
			name:     "manifest unknown in stream",
			status:   200,
			body:     `{"errorDetail":{"message":"manifest for nginx:nope not found: manifest unknown"},"error":"manifest for nginx:nope not found: manifest unknown"}` + "\n",
			wantNF:   true,
			wantText: "manifest unknown",
		},
		{
			name:     "access denied for a missing repository",
			status:   200,
			body:     `{"errorDetail":{"message":"pull access denied for nope, repository does not exist"},"error":"pull access denied"}` + "\n",
			wantNF:   true,
			wantText: "pull access denied",
		},
		{
			name:     "other error in stream",
			status:   200,
			body:     `{"status":"Pulling"}` + "\n" + `{"error":"write /var/lib/docker: no space left on device"}` + "\n",
			wantText: "no space left on device",
		},
		{
			name:     "not found status",
			status:   404,
			body:     `{"message":"no such image"}`,
			wantNF:   true,
			wantText: "no such image",
		},
		{
			name:     "server error status",
			status:   500,
			body:     `internal failure`,
			wantText: "internal failure",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := c.Pull(context.Background(), "nginx:nope")
			if err == nil {
				t.Fatal("Pull succeeded, want an error")
			}
			if tc.wantNF {
				requireIs(t, err, runtime.ErrNotFound)
			} else {
				requireNotIs(t, err, runtime.ErrNotFound)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not contain %q", err, tc.wantText)
			}
		})
	}
}

func TestPullRejectsBadReferenceWithoutRequest(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {})
	err := c.Pull(context.Background(), "Bad Name:!!")
	requireIs(t, err, runtime.ErrInvalid)
	if n := len(fd.requests()); n != 0 {
		t.Fatalf("%d requests sent for an invalid reference", n)
	}
}

func TestCreateRequestBody(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusCreated, map[string]any{"Id": "c0ffee", "Warnings": []string{}})
	})
	id, err := c.Create(context.Background(), runtime.ContainerConfig{
		Name:    "web-1-2",
		Image:   "nginx",
		Command: []string{"nginx", "-g", "daemon off;"},
		Env:     map[string]string{"ZED": "last", "ALPHA": "first"},
		Ports: []runtime.PortBinding{
			{HostPort: 8080, ContainerPort: 80},                                         // bind defaults to loopback
			{HostIP: "192.168.1.5", HostPort: 5353, ContainerPort: 53, Protocol: "udp"}, // explicit bind kept
		},
		Volumes:    []runtime.VolumeMount{{Name: "data", Path: "/usr/share/data"}},
		App:        "web",
		Replica:    1,
		Generation: 2,
	})
	requireNoErr(t, err)
	if id != "c0ffee" {
		t.Fatalf("id = %q", id)
	}
	reqs := fd.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/containers/create" {
		t.Fatalf("requests = %+v", reqs)
	}
	if got := reqs[0].Query.Get("name"); got != "web-1-2" {
		t.Errorf("name = %q", got)
	}

	var body struct {
		Image        string
		Cmd          []string
		Env          []string
		Labels       map[string]string
		Tty          bool
		ExposedPorts map[string]struct{}
		HostConfig   struct {
			PortBindings map[string][]struct{ HostIp, HostPort string }
			Mounts       []struct {
				Type, Source, Target string
				VolumeOptions        struct{ Labels map[string]string }
			}
			RestartPolicy struct{ Name string }
		}
	}
	if err := json.Unmarshal(reqs[0].Body, &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"image normalised", body.Image == "docker.io/library/nginx:latest"},
		{"command", strings.Join(body.Cmd, "|") == "nginx|-g|daemon off;"},
		{"env sorted", strings.Join(body.Env, ",") == "ALPHA=first,ZED=last"},
		{"labels", body.Labels[runtime.LabelApp] == "web" && body.Labels[runtime.LabelReplica] == "1" && body.Labels[runtime.LabelGeneration] == "2"},
		{"no tty", !body.Tty},
		{"exposed port", len(body.ExposedPorts) == 2 && hasKey(body.ExposedPorts, "80/tcp") && hasKey(body.ExposedPorts, "53/udp")},
		{"loopback default bind", len(body.HostConfig.PortBindings["80/tcp"]) == 1 &&
			body.HostConfig.PortBindings["80/tcp"][0].HostIp == "127.0.0.1" &&
			body.HostConfig.PortBindings["80/tcp"][0].HostPort == "8080"},
		{"explicit bind kept", len(body.HostConfig.PortBindings["53/udp"]) == 1 &&
			body.HostConfig.PortBindings["53/udp"][0].HostIp == "192.168.1.5"},
		{"volume scoped to the app", len(body.HostConfig.Mounts) == 1 &&
			body.HostConfig.Mounts[0].Type == "volume" &&
			body.HostConfig.Mounts[0].Source == "hivemind-web_data" &&
			body.HostConfig.Mounts[0].Target == "/usr/share/data"},
		{"volume labels", len(body.HostConfig.Mounts) == 1 &&
			len(body.HostConfig.Mounts[0].VolumeOptions.Labels) == 2 &&
			body.HostConfig.Mounts[0].VolumeOptions.Labels[runtime.LabelApp] == "web" &&
			body.HostConfig.Mounts[0].VolumeOptions.Labels[runtime.LabelVolume] == "data"},
		{"restart policy no", body.HostConfig.RestartPolicy.Name == "no"},
	}
	for _, ck := range checks {
		if !ck.ok {
			t.Errorf("%s: body = %s", ck.name, reqs[0].Body)
		}
	}
}

func TestCreateResourceLimits(t *testing.T) {
	tests := []struct {
		name   string
		cpu    float64
		memory int64
		// want is the HostConfig fields in the body; a missing key was left
		// out of the JSON.
		want map[string]int64
	}{
		{name: "no limits", want: map[string]int64{}},
		{name: "half a cpu", cpu: 0.5, want: map[string]int64{"NanoCpus": 500_000_000}},
		{name: "two cpus", cpu: 2, want: map[string]int64{"NanoCpus": 2_000_000_000}},
		{name: "a third of a cpu rounds", cpu: 1.0 / 3, want: map[string]int64{"NanoCpus": 333_333_333}},
		{name: "smallest cpu", cpu: 0.01, want: map[string]int64{"NanoCpus": 10_000_000}},
		{name: "memory without swap", memory: 256 << 20, want: map[string]int64{"Memory": 256 << 20, "MemorySwap": 256 << 20}},
		{name: "both", cpu: 1.5, memory: 1 << 30, want: map[string]int64{"NanoCpus": 1_500_000_000, "Memory": 1 << 30, "MemorySwap": 1 << 30}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
				writeJSON(w, http.StatusCreated, map[string]any{"Id": "c0ffee"})
			})
			_, err := c.Create(context.Background(), runtime.ContainerConfig{
				Name: "web", Image: "nginx", App: "web", CPU: tc.cpu, Memory: tc.memory,
			})
			requireNoErr(t, err)
			var body struct {
				HostConfig map[string]json.RawMessage
			}
			if err := json.Unmarshal(fd.requests()[0].Body, &body); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"NanoCpus", "Memory", "MemorySwap"} {
				raw, present := body.HostConfig[k]
				want, wantPresent := tc.want[k]
				if present != wantPresent {
					t.Errorf("%s present = %v, want %v: %s", k, present, wantPresent, fd.requests()[0].Body)
					continue
				}
				if !present {
					continue
				}
				var got int64
				if err := json.Unmarshal(raw, &got); err != nil || got != want {
					t.Errorf("%s = %s, want %d", k, raw, want)
				}
			}
		})
	}
}

func TestCreateRejectsResourcesWithoutRequest(t *testing.T) {
	tests := map[string]runtime.ContainerConfig{
		"negative cpu":         {CPU: -1},
		"cpu below minimum":    {CPU: 0.001},
		"cpu not a number":     {CPU: math.NaN()},
		"negative memory":      {Memory: -1},
		"memory below minimum": {Memory: 4 << 20},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {})
			cfg.Name, cfg.Image, cfg.App = "web", "nginx", "web"
			_, err := c.Create(context.Background(), cfg)
			requireIs(t, err, runtime.ErrInvalid)
			if n := len(fd.requests()); n != 0 {
				t.Fatalf("%d requests sent", n)
			}
		})
	}
}

func hasKey(m map[string]struct{}, k string) bool {
	_, ok := m[k]
	return ok
}

func TestCreateErrors(t *testing.T) {
	valid := runtime.ContainerConfig{Name: "web", Image: "nginx", App: "web"}
	tests := []struct {
		name    string
		status  int
		body    string
		cfg     func(c runtime.ContainerConfig) runtime.ContainerConfig
		wantIs  error
		noSend  bool // the config must be rejected before any request
		message string
	}{
		{name: "missing image", status: 404, body: `{"message":"No such image: nginx:nope"}`, wantIs: runtime.ErrNotFound, message: "No such image"},
		{name: "name in use", status: 409, body: `{"message":"Conflict. The container name \"/web\" is already in use"}`, wantIs: runtime.ErrConflict},
		{name: "server error", status: 500, body: `{"message":"boom"}`, message: "boom"},
		{
			name: "host port zero", noSend: true, wantIs: runtime.ErrInvalid,
			cfg: func(c runtime.ContainerConfig) runtime.ContainerConfig {
				c.Ports = []runtime.PortBinding{{HostPort: 0, ContainerPort: 80}}
				return c
			},
		},
		{
			name: "bind is not an address", noSend: true, wantIs: runtime.ErrInvalid,
			cfg: func(c runtime.ContainerConfig) runtime.ContainerConfig {
				c.Ports = []runtime.PortBinding{{HostIP: "localhost", HostPort: 80, ContainerPort: 80}}
				return c
			},
		},
		{
			name: "volume at root", noSend: true, wantIs: runtime.ErrInvalid,
			cfg: func(c runtime.ContainerConfig) runtime.ContainerConfig {
				c.Volumes = []runtime.VolumeMount{{Name: "v", Path: "/"}}
				return c
			},
		},
		{
			name: "empty app", noSend: true, wantIs: runtime.ErrInvalid,
			cfg: func(c runtime.ContainerConfig) runtime.ContainerConfig { c.App = ""; return c },
		},
		{
			name: "bad container name", noSend: true, wantIs: runtime.ErrInvalid,
			cfg: func(c runtime.ContainerConfig) runtime.ContainerConfig { c.Name = "-web"; return c },
		},
		{
			name: "negative replica", noSend: true, wantIs: runtime.ErrInvalid,
			cfg: func(c runtime.ContainerConfig) runtime.ContainerConfig { c.Replica = -1; return c },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			cfg := valid
			if tc.cfg != nil {
				cfg = tc.cfg(cfg)
			}
			_, err := c.Create(context.Background(), cfg)
			if err == nil {
				t.Fatal("Create succeeded, want an error")
			}
			if tc.wantIs != nil {
				requireIs(t, err, tc.wantIs)
			}
			if tc.message != "" && !strings.Contains(err.Error(), tc.message) {
				t.Errorf("error %q does not contain %q", err, tc.message)
			}
			if tc.noSend && len(fd.requests()) != 0 {
				t.Fatalf("request sent for an invalid config")
			}
		})
	}
}

func TestStartResults(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
		wantIs  error
		wantSub string
	}{
		{name: "started", status: 204},
		{name: "already running", status: 304},
		{name: "not found", status: 404, body: `{"message":"No such container: x"}`, wantErr: true, wantIs: runtime.ErrNotFound},
		{
			name:    "port already allocated",
			status:  500,
			body:    `{"message":"driver failed programming external connectivity: Bind for 127.0.0.1:8080 failed: port is already allocated"}`,
			wantErr: true, wantIs: runtime.ErrConflict, wantSub: "port is already allocated",
		},
		{
			name:    "port already allocated with 409",
			status:  409,
			body:    `{"message":"Bind for 0.0.0.0:80 failed: port is already allocated"}`,
			wantErr: true, wantIs: runtime.ErrConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			err := c.Start(context.Background(), "abc")
			if !tc.wantErr {
				requireNoErr(t, err)
			} else {
				if err == nil {
					t.Fatal("Start succeeded, want an error")
				}
				requireIs(t, err, tc.wantIs)
				if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
					t.Errorf("error %q does not contain %q", err, tc.wantSub)
				}
			}
			reqs := fd.requests()
			if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/containers/abc/start" {
				t.Fatalf("requests = %+v", reqs)
			}
		})
	}
}

func TestStopSendsRoundedTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{name: "rounded up", timeout: 1500 * time.Millisecond, want: "2"},
		{name: "whole seconds", timeout: 10 * time.Second, want: "10"},
		{name: "zero", timeout: 0, want: "0"},
		{name: "sub second", timeout: 200 * time.Millisecond, want: "1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
				w.WriteHeader(http.StatusNoContent)
			})
			requireNoErr(t, c.Stop(context.Background(), "abc", tc.timeout))
			reqs := fd.requests()
			if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/containers/abc/stop" {
				t.Fatalf("requests = %+v", reqs)
			}
			if got := reqs[0].Query.Get("t"); got != tc.want {
				t.Errorf("t = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStopResults(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container"})
	})
	requireIs(t, c.Stop(context.Background(), "gone", time.Second), runtime.ErrNotFound)

	c2, _ := newFake(t, func(w http.ResponseWriter, ex exchange) { w.WriteHeader(http.StatusNotModified) })
	requireNoErr(t, c2.Stop(context.Background(), "stopped", time.Second))

	c3, fd3 := newFake(t, func(w http.ResponseWriter, ex exchange) {})
	err := c3.Stop(context.Background(), "abc", -time.Second)
	requireIs(t, err, runtime.ErrInvalid)
	if n := len(fd3.requests()); n != 0 {
		t.Fatalf("%d requests for a negative timeout", n)
	}
}

func TestRemoveKeepsVolumesAndEscapesID(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) { w.WriteHeader(http.StatusNoContent) })
	requireNoErr(t, c.Remove(context.Background(), "a/b", true))
	requireNoErr(t, c.Remove(context.Background(), "plain", false))
	reqs := fd.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].Method != http.MethodDelete || reqs[0].Path != "/containers/a%2Fb" {
		t.Errorf("request 0 = %s %s", reqs[0].Method, reqs[0].Path)
	}
	if reqs[0].Query.Get("force") != "true" || reqs[0].Query.Get("v") != "false" {
		t.Errorf("query = %v, want force=true and v=false", reqs[0].Query)
	}
	if reqs[1].Query.Get("force") != "false" {
		t.Errorf("force = %q, want false", reqs[1].Query.Get("force"))
	}

	c404, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container"})
	})
	requireIs(t, c404.Remove(context.Background(), "gone", true), runtime.ErrNotFound)
}

func TestInspectParsesContainer(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusOK, map[string]any{
			"Id":   "abc123",
			"Name": "/web-1-2",
			"Config": map[string]any{
				"Image":  "docker.io/library/nginx:latest",
				"Labels": map[string]string{runtime.LabelApp: "web"},
			},
			"State": map[string]any{"Status": "exited", "ExitCode": 137, "OOMKilled": true},
			"HostConfig": map[string]any{
				"RestartPolicy": map[string]any{"Name": "no"},
			},
		})
	})
	got, err := c.Inspect(context.Background(), "abc123")
	requireNoErr(t, err)
	if got.ID != "abc123" || got.Name != "web-1-2" || got.Image != "docker.io/library/nginx:latest" {
		t.Errorf("container = %+v", got)
	}
	if got.State != "exited" || got.Running() || got.ExitCode != 137 || !got.OOMKilled {
		t.Errorf("state = %+v", got)
	}
	if got.RestartPolicy != "no" || got.Labels[runtime.LabelApp] != "web" {
		t.Errorf("restart policy or labels = %+v", got)
	}
	if reqs := fd.requests(); reqs[0].Method != http.MethodGet || reqs[0].Path != "/containers/abc123/json" {
		t.Errorf("request = %+v", reqs[0])
	}

	c404, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "No such container: x"})
	})
	_, err = c404.Inspect(context.Background(), "x")
	requireIs(t, err, runtime.ErrNotFound)
}

func TestListFiltersByLabel(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"Id": "1", "Names": []string{"/web-1-1"}, "Image": "nginx", "State": "running",
				"Labels": map[string]string{runtime.LabelApp: "web"}},
			{"Id": "2", "Names": []string{"/web-2-1"}, "Image": "nginx", "State": "exited",
				"Labels": map[string]string{runtime.LabelApp: "web"}},
		})
	})
	got, err := c.List(context.Background(), map[string]string{
		runtime.LabelApp:     "web",
		runtime.LabelReplica: "",
	})
	requireNoErr(t, err)
	if len(got) != 2 || got[0].Name != "web-1-1" || !got[0].Running() || got[1].Running() {
		t.Fatalf("list = %+v", got)
	}
	reqs := fd.requests()
	if reqs[0].Path != "/containers/json" || reqs[0].Query.Get("all") != "1" {
		t.Fatalf("request = %+v", reqs[0])
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(reqs[0].Query.Get("filters")), &filters); err != nil {
		t.Fatalf("filters %q: %v", reqs[0].Query.Get("filters"), err)
	}
	want := []string{"hivemind.app=web", "hivemind.replica"}
	if strings.Join(filters["label"], ",") != strings.Join(want, ",") {
		t.Errorf("label filter = %v, want %v", filters["label"], want)
	}
}

func TestInfoParses(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusOK, map[string]any{"NCPU": 4, "MemTotal": int64(8589934592), "ServerVersion": "29.0.1"})
	})
	info, err := c.Info(context.Background())
	requireNoErr(t, err)
	if info.NCPU != 4 || info.MemTotal != 8589934592 || info.ServerVersion != "29.0.1" {
		t.Fatalf("info = %+v", info)
	}
}

func TestLogsSeparatesStreamsAndSetsOptions(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		_, _ = w.Write(frame(1, "hello from stdout\n"))
		_, _ = w.Write(frame(2, "hello from stderr\n"))
	})
	var out, errOut strings.Builder
	err := c.Logs(context.Background(), "abc", runtime.LogOptions{Follow: true, Tail: 5}, &out, &errOut)
	requireNoErr(t, err)
	if out.String() != "hello from stdout\n" || errOut.String() != "hello from stderr\n" {
		t.Errorf("stdout = %q, stderr = %q", out.String(), errOut.String())
	}
	reqs := fd.requests()
	if reqs[0].Path != "/containers/abc/logs" {
		t.Fatalf("path = %s", reqs[0].Path)
	}
	q := reqs[0].Query
	if q.Get("follow") != "true" || q.Get("tail") != "5" || q.Get("stdout") != "1" || q.Get("stderr") != "1" {
		t.Errorf("query = %v", q)
	}

	_ = c.Logs(context.Background(), "abc", runtime.LogOptions{}, io.Discard, io.Discard)
	q = fd.requests()[1].Query
	if q.Get("follow") != "false" || q.Get("tail") != "all" {
		t.Errorf("default query = %v, want follow=false tail=all", q)
	}
	err = c.Logs(context.Background(), "abc", runtime.LogOptions{Tail: -1}, io.Discard, io.Discard)
	requireIs(t, err, runtime.ErrInvalid)
}

func TestLogsWriterErrorIsNotUnavailable(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		_, _ = w.Write(frame(1, "some output"))
	})
	err := c.Logs(context.Background(), "abc", runtime.LogOptions{}, failingWriter{}, io.Discard)
	if err == nil {
		t.Fatal("Logs succeeded with a failing writer")
	}
	requireNotIs(t, err, runtime.ErrUnavailable)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// hijack takes over the connection, writes raw bytes, and closes it.
func hijack(raw string) handler {
	return func(w http.ResponseWriter, ex exchange) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, raw)
	}
}

const rawStreamHeader = "HTTP/1.1 200 OK\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: close\r\n\r\n"

func TestDaemonDropsConnectionMidCall(t *testing.T) {
	t.Run("before any response", func(t *testing.T) {
		c, _ := newFake(t, hijack(""))
		_, err := c.Inspect(context.Background(), "abc")
		requireIs(t, err, runtime.ErrUnavailable)
		requireRuntimeError(t, err)
	})
	t.Run("logs cut inside a frame", func(t *testing.T) {
		partial := string(frame(1, "abcdef")[:10])
		c, _ := newFake(t, hijack(rawStreamHeader+partial))
		err := c.Logs(context.Background(), "abc", runtime.LogOptions{Follow: true}, io.Discard, io.Discard)
		requireIs(t, err, runtime.ErrUnavailable)
	})
	t.Run("logs cut on a frame boundary", func(t *testing.T) {
		c, _ := newFake(t, hijack(rawStreamHeader+string(frame(1, "whole line\n"))))
		var out strings.Builder
		err := c.Logs(context.Background(), "abc", runtime.LogOptions{Follow: true}, &out, io.Discard)
		// The stream ended cleanly, so this is the end of the logs, not a failure.
		requireNoErr(t, err)
		if out.String() != "whole line\n" {
			t.Errorf("stdout = %q", out.String())
		}
	})
}

func TestUnreachableDaemon(t *testing.T) {
	ctx := context.Background()

	missing, err := New("unix://" + t.TempDir() + "/missing.sock")
	requireNoErr(t, err)
	_, err = missing.Info(ctx)
	requireIs(t, err, runtime.ErrUnavailable)
	if re := requireRuntimeError(t, err); re.Op != "info" {
		t.Errorf("op = %q, want info", re.Op)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	requireNoErr(t, err)
	addr := l.Addr().String()
	requireNoErr(t, l.Close())
	closed, err := New("tcp://" + addr)
	requireNoErr(t, err)
	_, err = closed.Info(ctx)
	requireIs(t, err, runtime.ErrUnavailable)
}

func TestCancelledContextIsNotUnavailable(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) { writeJSON(w, http.StatusOK, map[string]int{}) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Info(ctx)
	requireIs(t, err, context.Canceled)
	requireNotIs(t, err, runtime.ErrUnavailable)
}

func TestErrorBodyIsKeptWhenNotJSON(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "proxy said no")
	})
	_, err := c.Info(context.Background())
	if err == nil {
		t.Fatal("Info succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "proxy said no") {
		t.Fatalf("error = %v", err)
	}
	requireNotIs(t, err, runtime.ErrNotFound)
}
