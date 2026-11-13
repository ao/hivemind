package runtime

import (
	"errors"
	"testing"
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
