package nodeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/store"
)

func TestValidateNodeAddress(t *testing.T) {
	for _, ok := range []string{"10.0.0.5", "192.168.1.20", "fd00::5", "2001:db8::1"} {
		if err := ValidateNodeAddress(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "127.0.0.1", "::1", "::ffff:127.0.0.1", "0.0.0.0", "::", "224.0.0.1", "ff02::1",
		"fe80::1%eth0", "node-1", "10.0.0.5:80", "010.0.0.5", "2001:DB8::1"} {
		if err := ValidateNodeAddress(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if err := (Register{Name: "w", Address: "127.0.0.1"}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("register with a loopback address: %v", err)
	}
	if err := (Register{Name: "w"}).Validate(); err != nil {
		t.Errorf("register without an address (a 2.1 worker): %v", err)
	}
}

func TestRouteTableValidate(t *testing.T) {
	good := RouteTable{Routes: []Route{
		{Host: "www.example.com", Endpoints: []string{"10.0.0.5:32768", "[fd00::5]:32769"}},
		{Host: "api.example.com", Endpoints: []string{}},
	}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, rt := range map[string]RouteTable{
		"wildcard host":  {Routes: []Route{{Host: "*.example.com"}}},
		"twice":          {Routes: []Route{{Host: "a.example.com"}, {Host: "a.example.com"}}},
		"no port":        {Routes: []Route{{Host: "a.example.com", Endpoints: []string{"10.0.0.5"}}}},
		"port zero":      {Routes: []Route{{Host: "a.example.com", Endpoints: []string{"10.0.0.5:0"}}}},
		"host name":      {Routes: []Route{{Host: "a.example.com", Endpoints: []string{"node-1:80"}}}},
		"wildcard addr":  {Routes: []Route{{Host: "a.example.com", Endpoints: []string{"0.0.0.0:80"}}}},
		"too many hosts": {Routes: make([]Route, maxRoutes+1)},
	} {
		if err := rt.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	rt := RouteTableFromStore([]store.Route{{Host: "www.example.com", App: "web", Port: 80}})
	b, _ := json.Marshal(rt)
	if !strings.Contains(string(b), `"endpoints":[]`) {
		t.Errorf("a host without endpoints encodes as %s", b)
	}
}

func TestTLSRequestValidate(t *testing.T) {
	ok := map[string]TLSRequest{
		TLSLoad:   {Key: "certificates/ca/www.example.com/www.example.com.crt"},
		TLSStore:  {Key: "acme/ca/users/a@example.com/a.key", Value: []byte("k")},
		TLSDelete: {Key: "certificates/ca/www.example.com"},
		TLSExists: {Key: "certificates"},
		TLSList:   {Key: "acme", Recursive: true},
		TLSStat:   {Key: "ocsp"},
		TLSLock:   {Name: "issue_cert_www.example.com", Owner: "a1b2c3-1", TTLMillis: 30000},
		TLSRenew:  {Name: "issue_cert_www.example.com", Owner: "a1b2c3-1", TTLMillis: 30000},
		TLSUnlock: {Name: "issue_cert_www.example.com", Owner: "a1b2c3-1"},
	}
	for op, r := range ok {
		if err := r.Validate(op); err != nil {
			t.Errorf("%s %+v: %v", op, r, err)
		}
	}
	bad := []struct {
		op  string
		req TLSRequest
	}{
		{TLSLoad, TLSRequest{Key: "hivemind.db"}},
		{TLSLoad, TLSRequest{Key: "acme/../../etc/passwd"}},
		{TLSStore, TLSRequest{Key: "acme"}},
		{TLSStore, TLSRequest{Key: "acme/x", Value: make([]byte, store.MaxTLSValueLen+1)}},
		{TLSDelete, TLSRequest{Key: ""}},
		{TLSList, TLSRequest{Key: "locks"}},
		{TLSLock, TLSRequest{Name: "x", Owner: "node-a/1", TTLMillis: 1000}},
		{TLSLock, TLSRequest{Name: "x", Owner: "o", TTLMillis: 0}},
		{TLSLock, TLSRequest{Name: "x", Owner: "o", TTLMillis: maxTLSTTL + 1}},
		{TLSLock, TLSRequest{Name: "", Owner: "o", TTLMillis: 1}},
		{TLSUnlock, TLSRequest{Name: "x", Owner: ""}},
		{TLSUnlock, TLSRequest{Name: "x", Owner: strings.Repeat("o", maxTLSOwnerLen+1)}},
		{"drop", TLSRequest{Key: "acme/x"}},
	}
	for _, c := range bad {
		if err := c.req.Validate(c.op); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s %+v accepted: %v", c.op, c.req, err)
		}
	}
	if err := (TLSReply{Keys: []string{"hivemind.db"}}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a reply with a key outside the namespace: %v", err)
	}
}

func TestStatusRoutePortsRoundTrip(t *testing.T) {
	sts := []store.Status{{Name: "hivemind-web-0-g1", App: "web", Generation: 1, State: "running", Ready: true,
		RoutePorts: []store.RoutePort{{Port: 80, HostPort: 32768}}}}
	rep := StatusFromStore(sts)
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := rep.ToStore(); len(got[0].RoutePorts) != 1 || got[0].RoutePorts[0] != sts[0].RoutePorts[0] {
		t.Fatalf("round trip: %+v", got)
	}
	rep.Containers[0].RoutePorts = []RoutePort{{Port: 80, HostPort: 0}}
	if err := rep.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("host port 0: %v", err)
	}
	rep.Containers[0].RoutePorts = make([]RoutePort, maxRoutePorts+1)
	if err := rep.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many route ports: %v", err)
	}
}

// FuzzDecodeRoutes covers the routing table a worker's router reads.
func FuzzDecodeRoutes(f *testing.F) {
	f.Add([]byte(`{"routes":[{"host":"www.example.com","endpoints":["10.0.0.5:32768"]}]}`))
	f.Add([]byte(`{"routes":[{"host":"www.example.com","endpoints":[]},{"host":"a.example.com","endpoints":["[fd00::1]:1"]}]}`))
	f.Add([]byte(`{"routes":[{"host":"*.example.com","endpoints":["x"]}]}`))
	f.Add([]byte(`{"routes":null}`))
	f.Add([]byte(`{"routes":[{"host":"www.example.com","endpoints":[]},{"host":"www.example.com","path":"/api","endpoints":["10.0.0.5:1"]}]}`))
	f.Add([]byte(`{"routes":[{"host":"www.example.com","path":"/a/../b","endpoints":[]},{"host":"www.example.com","path":"/a%2F","endpoints":[]}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var rt RouteTable
		if err := Decode(bytes.NewReader(data), MaxRoutesBytes, &rt); err != nil {
			return
		}
		if err := rt.Validate(); err != nil {
			return
		}
		b, err := json.Marshal(rt)
		if err != nil {
			t.Fatalf("a valid table does not encode: %v", err)
		}
		var again RouteTable
		if err := Decode(bytes.NewReader(b), MaxRoutesBytes, &again); err != nil {
			t.Fatalf("a valid table does not decode after encoding: %v", err)
		}
		if err := again.Validate(); err != nil {
			t.Fatalf("a valid table fails validation after a round trip: %v", err)
		}
	})
}

// FuzzDecodeTLSRequest covers the body of every TLS storage operation. A
// request that validates names a key inside certmagic's namespace.
func FuzzDecodeTLSRequest(f *testing.F) {
	f.Add("load", []byte(`{"key":"certificates/ca/www.example.com/www.example.com.key"}`))
	f.Add("store", []byte(`{"key":"acme/ca/users/a@example.com/a.json","value":"e30="}`))
	f.Add("list", []byte(`{"key":"acme","recursive":true}`))
	f.Add("lock", []byte(`{"name":"issue_cert_x","owner":"ab-1","ttl_ms":30000}`))
	f.Add("unlock", []byte(`{"name":"issue_cert_x","owner":"ab-1"}`))
	f.Add("load", []byte(`{"key":"../hivemind.db"}`))
	f.Fuzz(func(t *testing.T, op string, data []byte) {
		req, err := DecodeTLSRequest(bytes.NewReader(data), op)
		if err != nil {
			return
		}
		switch op {
		case TLSLoad, TLSStore:
			if err := store.ValidTLSKey(req.Key); err != nil {
				t.Fatalf("%s accepted key %q: %v", op, req.Key, err)
			}
		case TLSDelete, TLSExists, TLSList, TLSStat:
			if err := store.ValidTLSPrefix(req.Key); err != nil {
				t.Fatalf("%s accepted key %q: %v", op, req.Key, err)
			}
			if strings.Contains(req.Key, "..") && strings.Contains(req.Key, "/../") {
				t.Fatalf("%s accepted a key that climbs: %q", op, req.Key)
			}
		case TLSLock, TLSRenew, TLSUnlock:
			if strings.Contains(req.Owner, "/") {
				t.Fatalf("an owner with a slash was accepted: %q", req.Owner)
			}
		default:
			t.Fatalf("unknown op %q accepted", op)
		}
	})
}
