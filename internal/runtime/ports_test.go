package runtime

import (
	"errors"
	"testing"
)

func TestPrepareEphemeralPorts(t *testing.T) {
	in := ContainerConfig{Name: "web-0-1", Image: "nginx", App: "web", Ports: []PortBinding{
		{HostPort: 8080, ContainerPort: 80},
		{HostIP: "10.0.0.5", ContainerPort: 80, Ephemeral: true},
		{ContainerPort: 8080, Ephemeral: true},
	}}
	out, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []PortBinding{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "10.0.0.5", ContainerPort: 80, Protocol: "tcp", Ephemeral: true},
		{HostIP: "127.0.0.1", ContainerPort: 8080, Protocol: "tcp", Ephemeral: true},
	}
	for i := range want {
		if out.Ports[i] != want[i] {
			t.Errorf("ports[%d] = %+v, want %+v", i, out.Ports[i], want[i])
		}
	}
	if in.Ports[1].Protocol != "" {
		t.Error("the caller's ports were modified")
	}

	for name, p := range map[string]PortBinding{
		"host port set":  {HostPort: 80, ContainerPort: 80, Ephemeral: true},
		"no container":   {Ephemeral: true},
		"bind not an IP": {HostIP: "node-1", ContainerPort: 80, Ephemeral: true},
		"protocol":       {ContainerPort: 80, Protocol: "sctp", Ephemeral: true},
	} {
		c := in
		c.Ports = []PortBinding{p}
		if _, err := Prepare(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPublishedPort(t *testing.T) {
	c := Container{Ports: []PortBinding{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "10.0.0.5", HostPort: 32768, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "10.0.0.5", HostPort: 32769, ContainerPort: 53, Protocol: "udp"},
	}}
	if got := c.PublishedPort("10.0.0.5", 80, "tcp"); got != 32768 {
		t.Errorf("80/tcp on 10.0.0.5 = %d", got)
	}
	if got := c.PublishedPort("10.0.0.5", 53, "tcp"); got != 0 {
		t.Errorf("53/tcp = %d, want 0", got)
	}
	if got := c.PublishedPort("10.0.0.6", 80, "tcp"); got != 0 {
		t.Errorf("another address = %d, want 0", got)
	}
}
