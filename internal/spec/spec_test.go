package spec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func valid() App {
	a := App{
		Name:  "web",
		Image: "nginx:alpine",
		Ports: []Port{{Host: 8080, Container: 80}},
		Env:   map[string]string{"MODE": "prod"},
		Health: &Health{
			HTTP: &HTTPCheck{Port: 80},
		},
		Resources: Resources{CPU: 0.5, Memory: 256 << 20},
	}
	a.Normalize()
	return a
}

func TestNormalizeDefaults(t *testing.T) {
	a := valid()
	if a.Replicas != 1 {
		t.Errorf("replicas = %d, want 1", a.Replicas)
	}
	if p := a.Ports[0]; p.Bind != "127.0.0.1" || p.Protocol != "tcp" {
		t.Errorf("port defaults = %+v", p)
	}
	h := a.Health
	if h.Interval.D() != 10*time.Second || h.Timeout.D() != 2*time.Second || h.Retries != 3 || h.HTTP.Path != "/" {
		t.Errorf("health defaults = %+v", h)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*App)
		want string
	}{
		{"empty name", func(a *App) { a.Name = "" }, "name"},
		{"uppercase name", func(a *App) { a.Name = "Web" }, "name"},
		{"path in name", func(a *App) { a.Name = "../etc" }, "name"},
		{"long name", func(a *App) { a.Name = strings.Repeat("a", 33) }, "name"},
		{"no image", func(a *App) { a.Image = "" }, "image"},
		{"bad image", func(a *App) { a.Image = "nginx:" }, "image"},
		{"replicas", func(a *App) { a.Replicas = MaxReplicas + 1 }, "replicas"},
		{"negative replicas", func(a *App) { a.Replicas = -1 }, "replicas"},
		{"env key", func(a *App) { a.Env = map[string]string{"1X": "y"} }, "env"},
		{"env nul", func(a *App) { a.Env = map[string]string{"X": "a\x00b"} }, "NUL"},
		{"command nul", func(a *App) { a.Command = []string{"a\x00"} }, "command"},
		{"port zero", func(a *App) { a.Ports[0].Host = 0 }, "ports[0]"},
		{"port high", func(a *App) { a.Ports[0].Container = 70000 }, "ports[0]"},
		{"protocol", func(a *App) { a.Ports[0].Protocol = "sctp" }, "protocol"},
		{"bind", func(a *App) { a.Ports[0].Bind = "example.com" }, "bind"},
		{"duplicate port", func(a *App) { a.Ports = append(a.Ports, a.Ports[0]) }, "twice"},
		{"volume name", func(a *App) { a.Volumes = []Volume{{Name: "../x", Path: "/data"}} }, "volumes[0]"},
		{"volume path relative", func(a *App) { a.Volumes = []Volume{{Name: "d", Path: "data"}} }, "path"},
		{"volume path root", func(a *App) { a.Volumes = []Volume{{Name: "d", Path: "/"}} }, "path"},
		{"volume path dotdot", func(a *App) { a.Volumes = []Volume{{Name: "d", Path: "/a/../b"}} }, "path"},
		{"volume with replicas", func(a *App) { a.Replicas = 2; a.Volumes = []Volume{{Name: "d", Path: "/d"}} }, "only 1 replica"},
		{"cpu", func(a *App) { a.Resources.CPU = -1 }, "cpu"},
		{"memory", func(a *App) { a.Resources.Memory = -1 }, "memory"},
		{"two checks", func(a *App) { a.Health.TCP = &TCPCheck{Port: 80} }, "exactly one"},
		{"no check", func(a *App) { a.Health.HTTP = nil }, "exactly one"},
		{"check path", func(a *App) { a.Health.HTTP.Path = "x y" }, "path"},
		{"interval", func(a *App) { a.Health.Interval = Duration(time.Millisecond) }, "interval"},
		{"timeout", func(a *App) { a.Health.Timeout = Duration(time.Minute) }, "timeout"},
		{"retries", func(a *App) { a.Health.Retries = 11 }, "retries"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := valid()
			c.mod(&a)
			err := a.Validate()
			if err == nil {
				t.Fatalf("accepted")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestTemplateIgnoresReplicas(t *testing.T) {
	a, b := valid(), valid()
	b.Replicas = 5
	if a.Template() != b.Template() {
		t.Fatal("replica count changed the template")
	}
	b.Image = "nginx:1.27"
	if a.Template() == b.Template() {
		t.Fatal("image change did not change the template")
	}
}

func TestParseImage(t *testing.T) {
	cases := map[string]string{
		"nginx":                            "docker.io/library/nginx:latest",
		"nginx:alpine":                     "docker.io/library/nginx:alpine",
		"user/app:1.0":                     "docker.io/user/app:1.0",
		"ghcr.io/ao/app":                   "ghcr.io/ao/app:latest",
		"localhost:5000/app:dev":           "localhost:5000/app:dev",
		"localhost/app":                    "localhost/app:latest",
		"registry.example.com:443/a/b/c:1": "registry.example.com:443/a/b/c:1",
		"nginx@sha256:" + strings.Repeat("a", 64):     "docker.io/library/nginx@sha256:" + strings.Repeat("a", 64),
		"nginx:1.2@sha256:" + strings.Repeat("0", 64): "docker.io/library/nginx:1.2@sha256:" + strings.Repeat("0", 64),
		"my_org/my-app.v2__x:Tag_1.2-rc":              "docker.io/my_org/my-app.v2__x:Tag_1.2-rc",
	}
	for in, want := range cases {
		img, err := ParseImage(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if img.String() != want {
			t.Errorf("%q -> %q, want %q", in, img.String(), want)
		}
	}
	for _, bad := range []string{"", "Nginx", "nginx:", ":tag", "a//b", "nginx@sha256:abc", "-x", "x-", "a b", "nginx:-x", strings.Repeat("a", 256), "host:99999999/x"} {
		if _, err := ParseImage(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestByteSize(t *testing.T) {
	cases := map[string]ByteSize{"0": 0, "1024": 1024, "1k": 1000, "1Ki": 1024, "256Mi": 256 << 20, "2G": 2e9, "1Ti": 1 << 40, "10MB": 10e6}
	for in, want := range cases {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("%q -> %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.5G", "1X", "99999999999999999999", "9999999999Ti"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecodeJSONAndYAML(t *testing.T) {
	js := `{"name":"api","image":"ghcr.io/x/api:2","replicas":3,
	        "ports":[{"host":9000,"container":8080}],
	        "resources":{"cpu":0.25,"memory":"128Mi"},
	        "health":{"tcp":{"port":8080},"interval":"5s"}}`
	var a App
	if err := json.Unmarshal([]byte(js), &a); err != nil {
		t.Fatal(err)
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Resources.Memory != 128<<20 || a.Health.Interval.D() != 5*time.Second {
		t.Fatalf("decoded %+v", a)
	}
	y := "name: api\nimage: ghcr.io/x/api:2\nreplicas: 3\nports:\n  - {host: 9000, container: 8080}\nresources: {cpu: 0.25, memory: 128Mi}\nhealth: {tcp: {port: 8080}, interval: 5s}\n"
	var b App
	if err := yaml.Unmarshal([]byte(y), &b); err != nil {
		t.Fatal(err)
	}
	b.Normalize()
	if a.Template() != b.Template() || a.Replicas != b.Replicas {
		t.Fatalf("JSON and YAML decode differently:\n%s\n%s", a.Template(), b.Template())
	}
	if err := json.Unmarshal([]byte(`{"health":{"interval":5}}`), &b); err == nil {
		t.Fatal("numeric duration accepted")
	}
}
