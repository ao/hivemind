package runtime

import (
	"errors"
	"maps"
	"testing"
)

// A route binding is published on its own host port, like a spec port, and
// recorded in LabelRoutePorts. Before, it was ephemeral: the engine picked
// a new host port at each start of the container.
func TestPrepareRoutePorts(t *testing.T) {
	in := ContainerConfig{Name: "web-0-1", Image: "nginx", App: "web", Routed: true, Ports: []PortBinding{
		{HostPort: 8080, ContainerPort: 80},
		{HostIP: "10.0.0.5", HostPort: 30001, ContainerPort: 8080, Route: true},
		{HostIP: "10.0.0.5", HostPort: 30000, ContainerPort: 80, Route: true},
	}}
	out, err := Prepare(in)
	if err != nil {
		t.Fatal(err)
	}
	want := []PortBinding{
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "10.0.0.5", HostPort: 30001, ContainerPort: 8080, Protocol: "tcp", Route: true},
		{HostIP: "10.0.0.5", HostPort: 30000, ContainerPort: 80, Protocol: "tcp", Route: true},
	}
	for i := range want {
		if out.Ports[i] != want[i] {
			t.Errorf("ports[%d] = %+v, want %+v", i, out.Ports[i], want[i])
		}
	}
	if in.Ports[1].Protocol != "" {
		t.Error("the caller's ports were modified")
	}
	if got := out.Labels()[LabelRoutePorts]; got != "80:30000,8080:30001" {
		t.Errorf("%s = %q", LabelRoutePorts, got)
	}
	hp, ok := RouteHostPorts(out.Labels())
	if !ok || !maps.Equal(hp, map[int]int{80: 30000, 8080: 30001}) {
		t.Errorf("RouteHostPorts = %v, %v", hp, ok)
	}
	if _, set := (ContainerConfig{App: "web"}).Labels()[LabelRoutePorts]; set {
		t.Error("a container without route bindings has the label")
	}

	for name, ports := range map[string][]PortBinding{
		"no host port":       {{ContainerPort: 80, Route: true}},
		"no container":       {{HostPort: 30000, Route: true}},
		"bind not an IP":     {{HostIP: "node-1", HostPort: 30000, ContainerPort: 80, Route: true}},
		"protocol":           {{HostPort: 30000, ContainerPort: 80, Protocol: "sctp", Route: true}},
		"a spec port's port": {{HostPort: 30000, ContainerPort: 81}, {HostIP: "10.0.0.5", HostPort: 30000, ContainerPort: 80, Route: true}},
		"twice":              {{HostIP: "10.0.0.5", HostPort: 30000, ContainerPort: 80, Route: true}, {HostIP: "10.0.0.5", HostPort: 30000, ContainerPort: 81, Route: true}},
	} {
		c := in
		c.Ports = ports
		if _, err := Prepare(c); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRouteHostPortsMalformed(t *testing.T) {
	for _, v := range []string{"", "80", "80:", ":30000", "80:30000,", "x:1", "80:0", "0:30000", "80:65536", "80:30000,80:30001"} {
		if hp, ok := RouteHostPorts(map[string]string{LabelRoutePorts: v}); ok {
			t.Errorf("%q: %v, want not ok", v, hp)
		}
	}
	if _, ok := RouteHostPorts(nil); ok {
		t.Error("no label: ok")
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
