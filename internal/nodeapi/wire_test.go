package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

func TestDecodeRejectsUnknownFieldsTrailingDataAndEmptyBodies(t *testing.T) {
	cases := map[string]string{
		"unknown field": `{"name":"a","cpu":1,"memory":1,"extra":true}`,
		"trailing":      `{"name":"a"}{}`,
		"empty":         "  \n",
		"not json":      `name=a`,
	}
	for name, body := range cases {
		var g Register
		if err := Decode(strings.NewReader(body), MaxRegisterBytes, &g); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestDecodeRefusesBodiesOverTheLimit(t *testing.T) {
	body := `{"name":"` + strings.Repeat("a", 100) + `"}`
	var g Register
	if err := Decode(strings.NewReader(body), 50, &g); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if err := Decode(strings.NewReader(body), int64(len(body)), &g); err != nil {
		t.Fatalf("body exactly at the limit: %v", err)
	}
}

func TestValidateRegister(t *testing.T) {
	ok := Register{Name: "worker-1", CPU: 4, Memory: 8 << 30}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, g := range map[string]Register{
		"negative cpu":    {Name: "a", CPU: -1},
		"huge cpu":        {Name: "a", CPU: maxCPU + 1},
		"negative memory": {Name: "a", Memory: -1},
		"control in name": {Name: "a\nb"},
		"long name":       {Name: strings.Repeat("x", maxNameLen+1)},
		"server no port":  {Name: "a", Server: "hm.example.internal"},
		"server port 0":   {Name: "a", Server: "hm.example.internal:0"},
		"server no host":  {Name: "a", Server: ":4484"},
		"server space":    {Name: "a", Server: "hm example:4484"},
		"server unicode":  {Name: "a", Server: "hä.example:4484"},
		"server long":     {Name: "a", Server: strings.Repeat("x", maxServerAddrLen) + ":1"},
	} {
		if err := g.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestValidateAssignments(t *testing.T) {
	good := Assignment{App: "web", Index: 0, NodeID: "node-a", Generation: 1, Desired: "running", Spec: spec.App{Image: "nginx"}}
	if err := (AssignmentList{Assignments: []Assignment{good}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Assignment){
		"bad desired":  func(a *Assignment) { a.Desired = "stopped" },
		"bad app":      func(a *Assignment) { a.App = "Web_App" },
		"generation 0": func(a *Assignment) { a.Generation = 0 },
		"no image":     func(a *Assignment) { a.Spec.Image = "" },
		"negative idx": func(a *Assignment) { a.Index = -1 },
	} {
		a := good
		mut(&a)
		if err := (AssignmentList{Assignments: []Assignment{a}}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestValidateStatus(t *testing.T) {
	good := Container{Name: "hivemind-web-0-g1", App: "web", Replica: 0, Generation: 1, State: "running", Health: "healthy"}
	if err := (StatusReport{Containers: []Container{good}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Container){
		"empty name":   func(c *Container) { c.Name = "" },
		"long name":    func(c *Container) { c.Name = strings.Repeat("n", maxContainerLen+1) },
		"control name": func(c *Container) { c.Name = "a\x00" },
		"replica":      func(c *Container) { c.Replica = -1 },
		"generation":   func(c *Container) { c.Generation = -1 },
		"restarts":     func(c *Container) { c.RestartCount = -1 },
		"health":       func(c *Container) { c.Health = "fine" },
		"long error":   func(c *Container) { c.Error = strings.Repeat("e", maxErrorLen+1) },
	} {
		c := good
		mut(&c)
		if err := (StatusReport{Containers: []Container{c}}).Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
	many := make([]Container, maxContainers+1)
	if err := (StatusReport{Containers: many}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("too many containers: got %v, want ErrInvalid", err)
	}
}

// A status report survives the trip to the store and back.
func TestStatusRoundTrip(t *testing.T) {
	next := time.Unix(1_700_000_000, 0).UTC()
	in := []store.Status{{Name: "hivemind-web-0-g2", ContainerID: "abc", App: "web", Replica: 0, Generation: 2,
		State: "running", Error: "x", RestartCount: 3, Health: "unhealthy", Ready: false, CrashLooping: true, NextRestart: next}}
	rep := StatusFromStore(in)
	if err := rep.Validate(); err != nil {
		t.Fatal(err)
	}
	got := rep.ToStore()
	if len(got) != 1 || got[0].Name != in[0].Name || got[0].RestartCount != 3 || !got[0].NextRestart.Equal(next) ||
		got[0].Health != "unhealthy" || !got[0].CrashLooping {
		t.Fatalf("round trip changed the status: %+v", got)
	}
}

// Assignments survive the wire and come back as the store's form.
func TestAssignmentRoundTrip(t *testing.T) {
	in := []store.Assignment{{
		Replica: store.Replica{App: "web", Index: 1, NodeID: "node-a", Generation: 4, Desired: store.Removed, PendingReason: "why"},
		Spec:    spec.App{Name: "web", Image: "nginx:alpine", Replicas: 2},
	}}
	b, err := json.Marshal(AssignmentList{Assignments: FromStore(in, true)})
	if err != nil {
		t.Fatal(err)
	}
	var list AssignmentList
	if err := Decode(bytes.NewReader(b), MaxAssignmentsBytes, &list); err != nil {
		t.Fatal(err)
	}
	if err := list.Validate(); err != nil {
		t.Fatal(err)
	}
	got := list.Assignments[0].ToStore()
	if got.App != "web" || got.Index != 1 || got.Desired != store.Removed || got.Generation != 4 ||
		got.PendingReason != "why" || got.Spec.Image != "nginx:alpine" {
		t.Fatalf("round trip changed the assignment: %+v", got)
	}
}

// FuzzDecodeStatus checks that no byte string makes the status decoder or
// validator panic, and that a report that validates converts and encodes again.
func FuzzDecodeStatus(f *testing.F) {
	f.Add([]byte(`{"containers":[{"name":"hivemind-web-0-g1","app":"web","replica":0,"generation":1,"state":"running","ready":true}]}`))
	f.Add([]byte(`{"containers":[]}`))
	f.Add([]byte(`{"containers":[{"name":"x","next_restart":"2020-01-01T00:00:00Z"}]}`))
	f.Add([]byte(`{"containers":[{"name":"x","bogus":1}]}`))
	f.Add([]byte(`{}{}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var rep StatusReport
		if err := Decode(bytes.NewReader(data), MaxStatusBytes, &rep); err != nil {
			return
		}
		if err := rep.Validate(); err != nil {
			return
		}
		_ = rep.ToStore()
		b, err := json.Marshal(rep)
		if err != nil {
			t.Fatalf("a valid report does not encode: %v", err)
		}
		var again StatusReport
		if err := Decode(bytes.NewReader(b), MaxStatusBytes, &again); err != nil {
			t.Fatalf("a valid report does not decode after encoding: %v", err)
		}
		if err := again.Validate(); err != nil {
			t.Fatalf("a valid report fails validation after a round trip: %v", err)
		}
	})
}

// FuzzDecodeAssignments is the same check for the assignment list.
func FuzzDecodeAssignments(f *testing.F) {
	f.Add([]byte(`{"assignments":[{"app":"web","index":0,"node_id":"node-a","generation":1,"desired":"running","spec":{"name":"web","image":"nginx","replicas":1}}]}`))
	f.Add([]byte(`{"assignments":[]}`))
	f.Add([]byte(`{"assignments":[{"app":"p","index":0,"node_id":"node-a","generation":1,"desired":"running","spec":{"name":"p","image":"ghcr.io/a/b:1"},"registry_auth":{"host":"ghcr.io","username":"u","password":"p"}}]}`))
	f.Add([]byte(`{"assignments":[{"app":"p","generation":1,"desired":"running","spec":{"image":"x"},"registry_auth":{"host":"GHCR.io"}}]}`))
	f.Add([]byte(`{"assignments":[{"app":"web","desired":"removed","spec":{"image":"x"}}],"extra":1}`))
	f.Add([]byte(`{"assignments":[{"app":"web","index":-5,"generation":-1,"spec":{"image":""}}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var list AssignmentList
		if err := Decode(bytes.NewReader(data), MaxAssignmentsBytes, &list); err != nil {
			return
		}
		if err := list.Validate(); err != nil {
			return
		}
		for _, a := range list.Assignments {
			_ = a.ToStore()
		}
	})
}

// FuzzDecodeRegister covers the registration body.
func FuzzDecodeRegister(f *testing.F) {
	f.Add([]byte(`{"name":"w","cpu":2,"memory":1073741824}`))
	f.Add([]byte(`{"name":"w","cpu":-2}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var g Register
		if err := Decode(bytes.NewReader(data), MaxRegisterBytes, &g); err == nil {
			_ = g.Validate()
		}
	})
}

// A report whose text holds terminal escapes, as a compromised worker's
// could, is taken, with each control character a space, so that hivemind
// status prints none of them; a newline in a Docker error is kept as a
// space too, and the lease is renewed. A control character in the
// container's name is still refused.
func TestStatusTextIsMadePrintable(t *testing.T) {
	const esc = "\x1b]0;owned\x07\x1b[2J\u009b31m"
	rep := StatusReport{Containers: []Container{{Name: "hivemind-web-0-g1", ContainerID: "abc" + esc, App: "web" + esc,
		Generation: 1, State: "running" + esc, Error: "pull failed:\n" + esc + "\xff"}}}
	if err := rep.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := rep.ToStore()[0]
	for field, s := range map[string]string{"container ID": got.ContainerID, "app": got.App, "state": got.State, "error": got.Error} {
		if strings.ContainsAny(s, "\x1b\x07\n\u009b") || !utf8.ValidString(s) {
			t.Errorf("%s %q holds a control character", field, s)
		}
	}
	if got.Error != "pull failed:  ]0;owned  [2J 31m�" || got.State != "running ]0;owned  [2J 31m" {
		t.Errorf("error %q, state %q", got.Error, got.State)
	}
	rep.Containers[0].Name += "\x1b"
	if err := rep.Validate(); !errors.Is(err, ErrInvalid) {
		t.Errorf("a name with ESC: %v, want ErrInvalid", err)
	}
	if Printable("plain text, ü") != "plain text, ü" {
		t.Error("Printable changed printable text")
	}
}

// The bound the store keeps on a node's assignments is at least what the
// node API sends that node, every credential included, for specs, reasons
// and a credential whose characters JSON escapes; and it is below what a
// worker reads, with room for the rest of the reply.
func TestAssignmentBytesBoundTheReply(t *testing.T) {
	if store.MaxNodeAssignmentBytes+(1<<20) > MaxAssignmentsBytes {
		t.Fatalf("the store's bound, %d, leaves too little room under the worker's %d", store.MaxNodeAssignmentBytes, MaxAssignmentsBytes)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertNode(ctx, store.Node{ID: "node-1", Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRegistry(ctx, "reg.example.com", `u<"\`, `p<>&"\ü`+" x"); err != nil {
		t.Fatal(err)
	}
	odd := `<>&"\ü` + " é"
	for _, a := range []spec.App{
		{Name: "web", Image: "reg.example.com/app:1", Replicas: 3, Env: map[string]string{"A": odd, "B": "x"},
			Command: []string{"sh", "-c", "echo " + odd}, Ports: []spec.Port{{Host: 8080, Container: 80}},
			Health: &spec.Health{Exec: []string{"true", odd}}},
		{Name: "db", Image: "postgres:16", Replicas: 1, Volumes: []spec.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}},
		{Name: "api", Image: "reg.example.com/api:2", Replicas: 2},
	} {
		if _, err := st.PutApp(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// A scaled-down replica that never had a container goes at once.
	if _, err := st.Scale(ctx, "web", 1); err != nil {
		t.Fatal(err)
	}
	sizes, err := st.AssignmentBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	as, err := st.AssignmentsForNode(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	reply, err := json.Marshal(AssignmentList{Assignments: FromStore(as, true)})
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 4 || !strings.Contains(string(reply), `"registry_auth"`) {
		t.Fatalf("%d assignments, reply %.300s", len(as), reply)
	}
	if got, bound := int64(len(reply)), sizes["node-1"]; got > bound || bound > 3*got {
		t.Fatalf("the reply is %d bytes, the bound %d", got, bound)
	}
}

// The server address a worker dials is host:port, a name or an IP address.
func TestValidateServerAddress(t *testing.T) {
	for _, s := range []string{"hm.example.internal:4484", "10.0.0.5:4484", "[fd00::5]:4484", "localhost:1", "a:65535"} {
		if err := (Register{Name: "w", Server: s}).Validate(); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}
