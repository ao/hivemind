package nodeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// pathsRoute and pathsRouteTable are the routing table as a router of a
// build with paths and before protocols decodes it: a protocol field is
// unknown to it, and refused.
type pathsRoute struct {
	Host      string   `json:"host"`
	Path      string   `json:"path,omitempty"`
	Endpoints []string `json:"endpoints"`
}

type pathsRouteTable struct {
	Routes []pathsRoute `json:"routes"`
}

func decodeAsPathsRouter(b []byte) (pathsRouteTable, error) {
	var t pathsRouteTable
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	err := dec.Decode(&t)
	return t, err
}

var h2cTable = []store.Route{
	{Host: "grpc.example.com", App: "grpc", Port: 50051, Protocol: "h2c", Endpoints: []string{"10.0.0.7:40001"}},
	{Host: "only.example.com", App: "only", Port: 80, Endpoints: []string{"10.0.0.6:40000"}},
	{Host: "www.example.com", App: "web", Port: 80, Endpoints: []string{"10.0.0.5:32768"}},
	{Host: "www.example.com", Path: "/echo.Echo", App: "grpc", Port: 50051, Protocol: "h2c", Endpoints: []string{"10.0.0.7:40001"}},
}

// A router that asks for protocols gets each route's; one of a build before
// protocols gets none, and no endpoints for an h2c route, which it would
// otherwise send HTTP/1.1; one before paths gets no endpoints for a host
// whose route is h2c, as for a host with paths.
func TestRouteTableProtocolsByReader(t *testing.T) {
	rt, held := RouteTableFromStore(h2cTable, true)
	if err := rt.Validate(); err != nil || held != nil {
		t.Fatalf("%v, held %v", err, held)
	}
	b, _ := json.Marshal(rt)
	for _, want := range []string{
		`{"host":"grpc.example.com","protocol":"h2c","endpoints":["10.0.0.7:40001"]}`,
		`{"host":"only.example.com","endpoints":["10.0.0.6:40000"]}`,
		`{"host":"www.example.com","path":"/echo.Echo","protocol":"h2c","endpoints":["10.0.0.7:40001"]}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("no %s in %s", want, b)
		}
	}
	var back RouteTable
	if err := Decode(bytes.NewReader(b), MaxAssignmentsBytes, &back); err != nil || back.Validate() != nil || back.Routes[0].Protocol != "h2c" {
		t.Fatalf("decoded back: %+v, %v", back, err)
	}
	if _, err := decodeAsPathsRouter(b); err == nil {
		t.Fatal("a router before protocols would read a table with protocols")
	}

	rt, held = RouteTableFromStore(h2cTable, false)
	if !slices.Equal(held, []string{"grpc.example.com", "www.example.com/echo.Echo"}) {
		t.Fatalf("held %v", held)
	}
	b, _ = json.Marshal(rt)
	old, err := decodeAsPathsRouter(b)
	if err != nil {
		t.Fatalf("a router before protocols cannot read its table: %v\n%s", err, b)
	}
	got := map[string][]string{}
	for _, r := range old.Routes {
		got[r.Host+r.Path] = r.Endpoints
	}
	want := map[string][]string{"grpc.example.com": {}, "only.example.com": {"10.0.0.6:40000"},
		"www.example.com": {"10.0.0.5:32768"}, "www.example.com/echo.Echo": {}}
	if len(got) != len(want) {
		t.Fatalf("routes %v, want %v", got, want)
	}
	for k, eps := range want {
		if g, ok := got[k]; !ok || g == nil || !slices.Equal(g, eps) {
			t.Fatalf("route %s: %v, want %v (%s)", k, g, eps, b)
		}
	}

	ht, held, h2c := HostTableFromStore(h2cTable)
	if !slices.Equal(held, []string{"www.example.com"}) || !slices.Equal(h2c, []string{"grpc.example.com"}) {
		t.Fatalf("held %v, h2c %v", held, h2c)
	}
	b, _ = json.Marshal(ht)
	if _, err := decodeAsOldRouter(b); err != nil {
		t.Fatalf("a router before paths cannot read its table: %v\n%s", err, b)
	}
	for _, want := range []string{`{"host":"grpc.example.com","endpoints":[]}`, `{"host":"www.example.com","endpoints":[]}`,
		`{"host":"only.example.com","endpoints":["10.0.0.6:40000"]}`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("no %s in %s", want, b)
		}
	}

	bad := RouteTable{Routes: []Route{{Host: "a.example.com", Protocol: "h2", Endpoints: []string{}}}}
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "use http or h2c") {
		t.Fatalf("a protocol this build does not have: %v", err)
	}
	if err := (RouteTable{Routes: []Route{{Host: "a.example.com", Protocol: "http"}}}).Validate(); err == nil {
		t.Fatal("http on the wire, where it is left out")
	}
}

// Assignments carry no route protocol: the agent does not use it, and a
// worker of a build before protocols would refuse the whole list for it.
// The store's spec keeps it.
func TestAssignmentsCarryNoProtocol(t *testing.T) {
	a := spec.App{Name: "grpc", Image: "example/grpc:1", Replicas: 1,
		Routes: []spec.Route{{Host: "grpc.example.com", Port: 50051, Protocol: "h2c"}, {Host: "www.example.com", Port: 80}}}
	out := FromStore([]store.Assignment{{Replica: store.Replica{App: "grpc", NodeID: "node-a", Generation: 1, Desired: "running"}, Spec: a}}, true)
	b, _ := json.Marshal(AssignmentList{Assignments: out})
	if strings.Contains(string(b), "protocol") {
		t.Fatalf("an assignment has a protocol: %s", b)
	}
	if a.Routes[0].Protocol != "h2c" || len(out[0].Spec.Routes) != 2 || out[0].Spec.Routes[0].Port != 50051 {
		t.Fatalf("the store's spec %+v, the assignment's %+v", a.Routes, out[0].Spec.Routes)
	}
}
