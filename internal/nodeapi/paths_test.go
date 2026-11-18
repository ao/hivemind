package nodeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// oldRoute and oldRouteTable are the routing table as a router of a build
// before path routes decodes it, and oldValidate its check: unknown fields
// and a host listed twice are refused.
type oldRoute struct {
	Host      string   `json:"host"`
	Endpoints []string `json:"endpoints"`
}

type oldRouteTable struct {
	Routes []oldRoute `json:"routes"`
}

func decodeAsOldRouter(b []byte) (oldRouteTable, error) {
	var t oldRouteTable
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return t, err
	}
	seen := map[string]bool{}
	for _, r := range t.Routes {
		if err := spec.ValidateHost(r.Host); err != nil {
			return t, err
		}
		if seen[r.Host] {
			return t, fmt.Errorf("route %s is listed twice", r.Host)
		}
		seen[r.Host] = true
		for _, e := range r.Endpoints {
			if _, err := netip.ParseAddrPort(e); err != nil {
				return t, err
			}
		}
	}
	return t, nil
}

var storeTable = []store.Route{
	{Host: "api.example.com", Path: "/v1", App: "api", Port: 80, Endpoints: []string{"10.0.0.7:40002"}},
	{Host: "only.example.com", App: "only", Port: 80, Endpoints: []string{"10.0.0.6:40000"}},
	{Host: "www.example.com", App: "web", Port: 80, Endpoints: []string{"10.0.0.5:32768"}},
	{Host: "www.example.com", Path: "/api", App: "api", Port: 80, Endpoints: []string{"10.0.0.7:40002"}},
	{Host: "www.example.com", Path: "/api/v2", App: "v2", Port: 80},
}

func TestRouteTableWithPaths(t *testing.T) {
	rt, held := RouteTableFromStore(storeTable, true)
	if err := rt.Validate(); err != nil || held != nil {
		t.Fatal(err)
	}
	if len(rt.Routes) != 5 || rt.Routes[3].Path != "/api" || rt.Routes[4].Endpoints == nil {
		t.Fatalf("table %+v", rt)
	}
	b, _ := json.Marshal(rt)
	// A whole host route has no path field, as before paths.
	if !strings.Contains(string(b), `{"host":"only.example.com","endpoints":["10.0.0.6:40000"]}`) ||
		!strings.Contains(string(b), `{"host":"www.example.com","path":"/api","endpoints":["10.0.0.7:40002"]}`) {
		t.Fatalf("encoded as %s", b)
	}
	var back RouteTable
	if err := Decode(bytes.NewReader(b), MaxAssignmentsBytes, &back); err != nil || back.Validate() != nil || len(back.Routes) != 5 {
		t.Fatalf("decoded back: %+v, %v", back, err)
	}
	// A router of a build before paths refuses this table: that is why it
	// gets HostTableFromStore's.
	if _, err := decodeAsOldRouter(b); err == nil {
		t.Fatal("a router before paths would read a table with paths")
	}

	for name, rt := range map[string]RouteTable{
		"a path twice": {Routes: []Route{{Host: "a.example.com", Path: "/x"}, {Host: "a.example.com", Path: "/x"}}},
		"a bad path":   {Routes: []Route{{Host: "a.example.com", Path: "/x/../y"}}},
		"no slash":     {Routes: []Route{{Host: "a.example.com", Path: "x"}}},
		"a slash":      {Routes: []Route{{Host: "a.example.com", Path: "/"}}},
		"encoded":      {Routes: []Route{{Host: "a.example.com", Path: "/a%2Fb"}}},
	} {
		if err := rt.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	two := RouteTable{Routes: []Route{{Host: "a.example.com", Endpoints: []string{}}, {Host: "a.example.com", Path: "/x", Endpoints: []string{}}}}
	if err := two.Validate(); err != nil {
		t.Errorf("a host with a whole host route and a path: %v", err)
	}
}

// The table for a router before paths decodes as that router decodes it,
// and holds back every host with a path route.
func TestHostTableForARouterBeforePaths(t *testing.T) {
	ht, held, h2c := HostTableFromStore(storeTable)
	if !slices.Equal(held, []string{"api.example.com", "www.example.com"}) || h2c != nil {
		t.Fatalf("held %v, h2c %v", held, h2c)
	}
	b, _ := json.Marshal(ht)
	old, err := decodeAsOldRouter(b)
	if err != nil {
		t.Fatalf("a router before paths cannot read its table: %v\n%s", err, b)
	}
	got := map[string][]string{}
	for _, r := range old.Routes {
		got[r.Host] = r.Endpoints
	}
	want := map[string][]string{"api.example.com": {}, "only.example.com": {"10.0.0.6:40000"}, "www.example.com": {}}
	if len(got) != len(want) {
		t.Fatalf("hosts %v, want %v", got, want)
	}
	for h, eps := range want {
		if g, ok := got[h]; !ok || !slices.Equal(g, eps) || g == nil {
			t.Fatalf("host %s: %v, want %v (%s)", h, g, eps, b)
		}
	}
	// A table without paths is given as it is.
	plain := []store.Route{{Host: "only.example.com", App: "only", Port: 80, Endpoints: []string{"10.0.0.6:40000"}}}
	ht, held, h2c = HostTableFromStore(plain)
	if len(held) != 0 || len(h2c) != 0 || len(ht.Routes) != 1 || !slices.Equal(ht.Routes[0].Endpoints, plain[0].Endpoints) {
		t.Fatalf("a table without paths: %+v, %v", ht, held)
	}
	if rt, _ := RouteTableFromStore(plain, false); !bytes.Equal(mustJSON(t, rt), mustJSON(t, ht)) {
		t.Fatalf("without paths, the two tables differ: %s", mustJSON(t, rt))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRouteReaders(t *testing.T) {
	var none *RouteReaders
	if none.Seen("node-a", RouteRead{Version: "v"}, time.Now()) || none.Old(time.Now()) != nil {
		t.Fatal("a nil RouteReaders keeps something")
	}
	rr := NewRouteReaders()
	t0 := time.Unix(1000, 0)
	beforePaths := RouteRead{Version: "v2.2.0", Held: []string{"www.example.com"}}
	if !rr.Seen("node-b", beforePaths, t0) {
		t.Fatal("a first read is not a change")
	}
	if rr.Seen("node-b", beforePaths, t0.Add(time.Second)) {
		t.Fatal("the same read again is a change")
	}
	if !rr.Seen("node-b", RouteRead{Version: "v2.2.0", Held: []string{"api.example.com", "www.example.com"}}, t0.Add(2*time.Second)) {
		t.Fatal("new held hosts are not a change")
	}
	rr.Seen("node-a", RouteRead{}, t0)
	rr.Seen("node-c", RouteRead{Version: "v2.3.0", Paths: true, Protocols: true}, t0)
	rr.Seen("node-d", RouteRead{Version: "v2.2.0-paths", Paths: true}, t0)
	old := rr.Old(t0.Add(30 * time.Second))
	if len(old) != 3 || old[0].Node != "node-a" || old[0].Hosts == nil || old[0].H2C == nil || old[1].Node != "node-b" ||
		!slices.Equal(old[1].Hosts, []string{"api.example.com", "www.example.com"}) || old[1].Version != "v2.2.0" || old[1].Paths ||
		old[2].Node != "node-d" || !old[2].Paths || old[2].Protocols || len(old[2].H2C) != 0 {
		t.Fatalf("old routers %+v", old)
	}
	// A router before protocols that is held back from an h2c route: a
	// change, and listed with the route.
	if !rr.Seen("node-d", RouteRead{Version: "v2.2.0-paths", Paths: true, H2C: []string{"grpc.example.com"}}, t0.Add(31*time.Second)) {
		t.Fatal("a new held h2c route is not a change")
	}
	if old := rr.Old(t0.Add(31 * time.Second)); len(old) != 3 || !slices.Equal(old[2].H2C, []string{"grpc.example.com"}) {
		t.Fatalf("old routers %+v", old)
	}
	// An upgraded router reads paths and protocols, and is no longer old.
	if !rr.Seen("node-b", RouteRead{Version: "v2.3.0", Paths: true, Protocols: true}, t0.Add(40*time.Second)) {
		t.Fatal("reading paths is not a change")
	}
	if !rr.Seen("node-d", RouteRead{Version: "v2.3.0", Paths: true, Protocols: true}, t0.Add(40*time.Second)) {
		t.Fatal("reading protocols is not a change")
	}
	if old := rr.Old(t0.Add(40 * time.Second)); len(old) != 1 || old[0].Node != "node-a" {
		t.Fatalf("old routers after the upgrades: %+v", old)
	}
	// A router that stopped reading is forgotten after RouteReaderMaxAge.
	if old := rr.Old(t0.Add(40*time.Second + RouteReaderMaxAge)); len(old) != 0 {
		t.Fatalf("old routers after they stopped: %+v", old)
	}
}
