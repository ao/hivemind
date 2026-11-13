package runtime

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
)

func TestPrepareAppliesDefaultsAndCopies(t *testing.T) {
	in := ContainerConfig{
		Name:    "web-1-1",
		Image:   "nginx",
		Env:     map[string]string{"A": "1"},
		Ports:   []PortBinding{{HostPort: 8080, ContainerPort: 80}},
		Volumes: []VolumeMount{{Name: "data", Path: "/data"}},
		App:     "web",
	}
	out, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Image != "docker.io/library/nginx:latest" {
		t.Errorf("image = %q", out.Image)
	}
	if out.Ports[0].HostIP != "127.0.0.1" || out.Ports[0].Protocol != "tcp" {
		t.Errorf("port = %+v, want loopback and tcp", out.Ports[0])
	}
	if in.Ports[0].HostIP != "" || in.Ports[0].Protocol != "" {
		t.Errorf("caller's ports were modified: %+v", in.Ports[0])
	}
}

func TestPrepareRejects(t *testing.T) {
	base := ContainerConfig{Name: "web", Image: "nginx", App: "web"}
	tests := map[string]func(c ContainerConfig) ContainerConfig{
		"empty name":      func(c ContainerConfig) ContainerConfig { c.Name = ""; return c },
		"name with slash": func(c ContainerConfig) ContainerConfig { c.Name = "a/b"; return c },
		"bad image":       func(c ContainerConfig) ContainerConfig { c.Image = "Bad Image!"; return c },
		"empty app":       func(c ContainerConfig) ContainerConfig { c.App = ""; return c },
		"env key":         func(c ContainerConfig) ContainerConfig { c.Env = map[string]string{"1X": "v"}; return c },
		"port out of range": func(c ContainerConfig) ContainerConfig {
			c.Ports = []PortBinding{{HostPort: 70000, ContainerPort: 80}}
			return c
		},
		"sctp protocol": func(c ContainerConfig) ContainerConfig {
			c.Ports = []PortBinding{{HostPort: 1, ContainerPort: 1, Protocol: "sctp"}}
			return c
		},
		"relative volume": func(c ContainerConfig) ContainerConfig {
			c.Volumes = []VolumeMount{{Name: "v", Path: "data"}}
			return c
		},
		"negative generation": func(c ContainerConfig) ContainerConfig { c.Generation = -1; return c },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Prepare(mutate(base))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	got := ContainerConfig{App: "web", Replica: 3, Generation: 7}.Labels()
	want := map[string]string{LabelApp: "web", LabelReplica: "3", LabelGeneration: "7"}
	if len(got) != len(want) {
		t.Fatalf("labels = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %s = %q, want %q", k, got[k], v)
		}
	}
}

// dockerVolumeNameRE is the engine's rule for volume names (RestrictedNamePattern).
var dockerVolumeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

func TestVolumeName(t *testing.T) {
	long := strings.Repeat("a", 31) + "z" // 32 characters, the spec's limit
	tests := []struct{ app, volume, want string }{
		{"web", "data", "hivemind-web_data"},
		{"a-b", "c", "hivemind-a-b_c"},
		{"a", "b-c", "hivemind-a_b-c"},
		{"0", "1", "hivemind-0_1"},
		{long, long, "hivemind-" + long + "_" + long},
	}
	for _, tc := range tests {
		if !spec.ValidName(tc.app) || !spec.ValidName(tc.volume) {
			t.Fatalf("test names %q, %q are not valid spec names", tc.app, tc.volume)
		}
		got := VolumeName(tc.app, tc.volume)
		if got != tc.want {
			t.Errorf("VolumeName(%q, %q) = %q, want %q", tc.app, tc.volume, got, tc.want)
		}
		if !dockerVolumeNameRE.MatchString(got) {
			t.Errorf("%q is not a valid Docker volume name", got)
		}
	}
	if n := len(VolumeName(long, long)); n != 74 {
		t.Errorf("longest name is %d characters, want 74", n)
	}
}

// Every pair of valid names maps to its own volume. The names are all the
// spec names of up to 3 characters over "a", "-" and "0", so dashes fall in
// every position the spec allows.
func TestVolumeNameIsInjective(t *testing.T) {
	var names []string
	var gen func(prefix string)
	gen = func(prefix string) {
		if spec.ValidName(prefix) {
			names = append(names, prefix)
		}
		if len(prefix) == 3 {
			return
		}
		for _, c := range []string{"a", "-", "0"} {
			gen(prefix + c)
		}
	}
	gen("")
	seen := map[string][2]string{}
	for _, app := range names {
		for _, vol := range names {
			got := VolumeName(app, vol)
			if prev, ok := seen[got]; ok {
				t.Fatalf("%q and %q both map to %q", prev, [2]string{app, vol}, got)
			}
			seen[got] = [2]string{app, vol}
		}
	}
	if len(seen) < 100 {
		t.Fatalf("only %d pairs checked", len(seen))
	}
}

func TestVolumeLabels(t *testing.T) {
	got := VolumeLabels("web", "data")
	if len(got) != 2 || got[LabelApp] != "web" || got[LabelVolume] != "data" {
		t.Fatalf("labels = %v", got)
	}
}

func TestErrorMatchesSentinelAndCause(t *testing.T) {
	cause := errors.New("connection refused")
	err := error(&Error{Op: "info", Err: ErrUnavailable, Message: "daemon down", Cause: cause})
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
		t.Fatalf("errors.Is does not see the sentinel and the cause: %v", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("matched ErrNotFound")
	}
	want := "info: daemon down: connection refused"
	if err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
}
