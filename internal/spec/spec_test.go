package spec

import (
	"encoding/json"
	"strconv"
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
		{"bind mount", func(a *App) { a.Volumes = []Volume{{Name: "/srv/data", Path: "/data"}} }, `volumes[0]: name "/srv/data" is a path on the host, and bind mounts are not supported`},
		{"relative bind mount", func(a *App) { a.Volumes = []Volume{{Name: "./data", Path: "/data"}} }, "bind mounts are not supported"},
		{"home bind mount", func(a *App) { a.Volumes = []Volume{{Name: "~/data", Path: "/data"}} }, "bind mounts are not supported"},
		{"dotted volume name", func(a *App) { a.Volumes = []Volume{{Name: "data.v2", Path: "/data"}} }, "use lowercase letters, digits and dashes"},
		{"volume path relative", func(a *App) { a.Volumes = []Volume{{Name: "d", Path: "data"}} }, "path"},
		{"volume path root", func(a *App) { a.Volumes = []Volume{{Name: "d", Path: "/"}} }, "path"},
		{"volume path dotdot", func(a *App) { a.Volumes = []Volume{{Name: "d", Path: "/a/../b"}} }, "path"},
		{"volume with replicas", func(a *App) { a.Replicas = 2; a.Volumes = []Volume{{Name: "d", Path: "/d"}} }, "only 1 replica"},
		{"cpu", func(a *App) { a.Resources.CPU = -1 }, "cpu"},
		{"memory", func(a *App) { a.Resources.Memory = -1 }, "memory"},
		{"cpu below minimum", func(a *App) { a.Resources.CPU = 0.005 }, "at least 0.01"},
		{"memory below minimum", func(a *App) { a.Resources.Memory = 6<<20 - 1 }, "at least 6Mi"},
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
	// Resources are container limits, so a change to them is a new template
	// and a rolling update.
	c, d := valid(), valid()
	d.Resources.CPU = 1
	if c.Template() == d.Template() {
		t.Fatal("cpu change did not change the template")
	}
	d = valid()
	d.Resources.Memory = 512 << 20
	if c.Template() == d.Template() {
		t.Fatal("memory change did not change the template")
	}
}

func TestResourceMinimums(t *testing.T) {
	for _, r := range []Resources{{}, {CPU: MinCPU}, {Memory: MinMemory}, {CPU: 0.01, Memory: 6 << 20}} {
		a := valid()
		a.Resources = r
		if err := a.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", r, err)
		}
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
		// Docker reads index.docker.io as docker.io, and so adds library/.
		"index.docker.io/nginx":      "docker.io/library/nginx:latest",
		"index.docker.io/user/app:1": "docker.io/user/app:1",
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

// The registry of an image, whose credential a pull sends, follows Docker's
// rules: the first component is a registry when it has a dot or a colon, is
// localhost, or has an upper-case letter; otherwise the image is on Docker
// Hub. Docker Hub's three names are one host.
func TestImageRegistry(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("b", 64)
	cases := []struct{ ref, want string }{
		{"nginx", "docker.io"},
		{"nginx:alpine", "docker.io"},
		{"user/app:1", "docker.io"},
		{"myregistry/app", "docker.io"}, // a Docker Hub namespace, not a host
		{"docker.io/library/nginx", "docker.io"},
		{"index.docker.io/nginx", "docker.io"},
		{"registry-1.docker.io/library/nginx:1", "docker.io"},
		{"ghcr.io/o/r:tag", "ghcr.io"},
		{"localhost/app", "localhost"},
		{"localhost:5000/app", "localhost:5000"},
		{"localhost:5000/app:dev", "localhost:5000"},
		{"127.0.0.1:5000/app", "127.0.0.1:5000"},
		{"host:5000/a/b" + digest, "host:5000"},
		{"host:5000/a/b:1" + digest, "host:5000"},
		{"registry.example.com:443/a/b/c:1", "registry.example.com:443"},
		{"Registry.Example.COM/app", "registry.example.com"},
		{"MyRegistry/app", "myregistry"}, // upper case makes it a host, as in Docker
	}
	for _, c := range cases {
		got, err := RegistryHost(c.ref)
		if err != nil || got != c.want {
			t.Errorf("RegistryHost(%q) = %q, %v; want %q", c.ref, got, err, c.want)
		}
	}
	if _, err := RegistryHost("Bad Name"); err == nil {
		t.Error("RegistryHost of an invalid reference succeeded")
	}
}

func TestParseRegistryHost(t *testing.T) {
	ok := map[string]string{
		"ghcr.io":                  "ghcr.io",
		"GHCR.io":                  "ghcr.io",
		"docker.io":                "docker.io",
		"index.docker.io":          "docker.io",
		"registry-1.docker.io":     "docker.io",
		"localhost":                "localhost",
		"localhost:5000":           "localhost:5000",
		"registry.example.com:443": "registry.example.com:443",
		"10.0.0.5:5000":            "10.0.0.5:5000",
	}
	for in, want := range ok {
		got, err := ParseRegistryHost(in)
		if err != nil || got != want {
			t.Errorf("ParseRegistryHost(%q) = %q, %v; want %q", in, got, err, want)
		}
		// The host of a credential is the registry of an image on it.
		if img, err := RegistryHost(in + "/app"); err != nil || img != got {
			t.Errorf("an image on %q has registry %q, %v; the credential is for %q", in, img, err, got)
		}
	}
	bad := map[string]string{
		"":                "required",
		"https://ghcr.io": "without a scheme",
		"ghcr.io/":        "without a path",
		"ghcr.io/org":     "without a path",
		"myregistry":      "repository on Docker Hub",
		"ghcr.io:0":       "port must be",
		"ghcr.io:65536":   "port must be",
		"ghcr.io:":        "want a host name",
		"-bad.io":         "want a host name",
		"a b.io":          "want a host name",
		"[::1]:5000":      "want a host name",
		"ghcr.io:5000:1":  "want a host name",
		"ghcr.io\n":       "want a host name",
		"user@ghcr.io":    "want a host name",
		"MyRegistry":      "repository on Docker Hub",
	}
	bad[strings.Repeat("a", 253)+".io"] = "longer than"
	for in, want := range bad {
		if _, err := ParseRegistryHost(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseRegistryHost(%q): %v, want an error with %q", in, err, want)
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

func TestRoutes(t *testing.T) {
	a := valid()
	a.Routes = []Route{{Host: " WWW.Example.COM. ", Port: 80}, {Host: "api.example.com", Port: 8080}, {Host: "xn--bcher-kva.example", Port: 80}}
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatalf("valid routes rejected: %v", err)
	}
	if a.Routes[0].Host != "www.example.com" {
		t.Errorf("host normalised to %q", a.Routes[0].Host)
	}
	if got := a.RoutePorts(); len(got) != 2 || got[0] != 80 || got[1] != 8080 {
		t.Errorf("RoutePorts = %v, want [80 8080]", got)
	}
	if got := a.RouteHosts(); len(got) != 3 || got[1] != "api.example.com" {
		t.Errorf("RouteHosts = %v", got)
	}
	b := a
	b.Routes = nil
	if a.Template() == b.Template() {
		t.Error("routes are not part of the template")
	}
	c := valid()
	c.Routes = []Route{}
	c.Normalize()
	if c.Routes != nil || c.Template() != valid().Template() {
		t.Error("an empty route list changed the spec")
	}
}

func TestRoutesRejected(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"", 80, "required"},
		{"*.example.com", 80, "wildcard"},
		{"www.*.com", 80, "wildcard"},
		{"10.0.0.1", 80, "IP address"},
		{"::1", 80, "IP address"},
		{"[::1]", 80, "IP address"},
		{"localhost", 80, "two labels"},
		{"www..example.com", 80, "label"},
		{"-www.example.com", 80, "dash"},
		{"www-.example.com", 80, "dash"},
		{"www_x.example.com", 80, "lowercase letters"},
		{"www.example.com:443", 80, "lowercase letters"},
		{"www.example.com/path", 80, "lowercase letters"},
		{"bücher.example", 80, "lowercase letters"},
		{"www.example.123", 80, "all digits"},
		{strings.Repeat("a", 64) + ".example.com", 80, "63"},
		{strings.Repeat("a.", 127) + "com", 80, "longer than"},
		{"www.example.com", 0, "port"},
		{"www.example.com", 65536, "port"},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			a := valid()
			a.Routes = []Route{{Host: c.host, Port: c.port}}
			a.Normalize()
			err := a.Validate()
			if err == nil {
				t.Fatalf("accepted %q", c.host)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
	a := valid()
	a.Routes = []Route{{Host: "www.example.com", Port: 80}, {Host: "WWW.example.com", Port: 81}}
	a.Normalize()
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate host: %v", err)
	}
	a = valid()
	for i := range MaxRoutes + 1 {
		a.Routes = append(a.Routes, Route{Host: "h" + strconv.Itoa(i) + ".example.com", Port: 80})
	}
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("too many routes: %v", err)
	}
}

func TestRoutesFromYAML(t *testing.T) {
	var a App
	if err := yaml.Unmarshal([]byte("name: web\nimage: nginx\nreplicas: 2\nroutes:\n  - host: www.example.com\n    port: 80\n"), &a); err != nil {
		t.Fatal(err)
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(a.Routes) != 1 || a.Routes[0] != (Route{Host: "www.example.com", Port: 80}) {
		t.Fatalf("routes = %+v", a.Routes)
	}
}
