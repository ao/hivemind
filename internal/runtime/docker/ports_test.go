package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ao/hivemind/internal/runtime"
)

// An ephemeral binding is sent with an empty HostPort, which asks the engine
// for a free port, beside a fixed binding of the same container port.
func TestCreateEphemeralPort(t *testing.T) {
	c, fd := newFake(t, func(w http.ResponseWriter, ex exchange) {
		writeJSON(w, http.StatusCreated, map[string]any{"Id": "c0ffee"})
	})
	_, err := c.Create(context.Background(), runtime.ContainerConfig{Name: "web-0-1", Image: "nginx", App: "web",
		Ports: []runtime.PortBinding{
			{HostPort: 8080, ContainerPort: 80},
			{HostIP: "10.0.0.5", ContainerPort: 80, Ephemeral: true},
		}})
	requireNoErr(t, err)
	var body struct {
		ExposedPorts map[string]struct{}
		HostConfig   struct {
			PortBindings map[string][]struct{ HostIp, HostPort string }
		}
	}
	if err := json.Unmarshal(fd.requests()[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	bs := body.HostConfig.PortBindings["80/tcp"]
	if len(body.ExposedPorts) != 1 || len(bs) != 2 || bs[0].HostPort != "8080" || bs[0].HostIp != "127.0.0.1" ||
		bs[1].HostPort != "" || bs[1].HostIp != "10.0.0.5" {
		t.Fatalf("bindings = %+v, exposed = %v", bs, body.ExposedPorts)
	}
}

func TestInspectAndListReadPublishedPorts(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, ex exchange) {
		if ex.Path == "/containers/json" {
			writeJSON(w, http.StatusOK, []map[string]any{{
				"Id": "1", "Names": []string{"/web-0-1"}, "State": "running",
				"Ports": []map[string]any{
					{"IP": "10.0.0.5", "PrivatePort": 80, "PublicPort": 32768, "Type": "tcp"},
					{"PrivatePort": 9000, "Type": "tcp"}, // exposed, not published
					{"IP": "127.0.0.1", "PrivatePort": 80, "PublicPort": 8080, "Type": "tcp"},
				},
			}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"Id": "1", "Name": "/web-0-1", "State": map[string]any{"Status": "running"},
			"NetworkSettings": map[string]any{"Ports": map[string]any{
				"80/tcp":   []map[string]string{{"HostIp": "10.0.0.5", "HostPort": "32768"}, {"HostIp": "127.0.0.1", "HostPort": "8080"}},
				"9000/tcp": nil,
				"bad":      []map[string]string{{"HostIp": "10.0.0.5", "HostPort": "1"}},
			}},
		})
	})
	want := []runtime.PortBinding{
		{HostIP: "10.0.0.5", HostPort: 32768, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
	}
	got, err := c.Inspect(context.Background(), "1")
	requireNoErr(t, err)
	if len(got.Ports) != 2 || got.Ports[0] != want[0] || got.Ports[1] != want[1] {
		t.Fatalf("inspect ports = %+v", got.Ports)
	}
	list, err := c.List(context.Background(), nil)
	requireNoErr(t, err)
	if len(list) != 1 || len(list[0].Ports) != 2 || list[0].Ports[0] != want[0] || list[0].Ports[1] != want[1] {
		t.Fatalf("list ports = %+v", list)
	}
	if list[0].PublishedPort("10.0.0.5", 80, "tcp") != 32768 {
		t.Fatal("PublishedPort on the listed container")
	}
}
