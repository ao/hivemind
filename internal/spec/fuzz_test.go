package spec

import (
	"encoding/json"
	"testing"
)

// FuzzAppJSON feeds arbitrary bytes through the same decode, normalise and
// validate path the API uses. It must never panic, and a spec that validates
// must survive a JSON round trip unchanged.
func FuzzAppJSON(f *testing.F) {
	f.Add([]byte(`{"name":"web","image":"nginx","ports":[{"host":80,"container":80}]}`))
	f.Add([]byte(`{"name":"db","image":"postgres:16","volumes":[{"name":"pg","path":"/var/lib/postgresql/data"}],"resources":{"memory":"1Gi"}}`))
	f.Add([]byte(`{"name":"x","image":"a/b:c","health":{"http":{"port":1,"path":"/h"},"interval":"1s","timeout":"1s"}}`))
	f.Add([]byte(`{"replicas":-5,"env":{"":"\u0000"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var a App
		if json.Unmarshal(data, &a) != nil {
			return
		}
		a.Normalize()
		if a.Validate() != nil {
			return
		}
		out, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal valid spec: %v", err)
		}
		var b App
		if err := json.Unmarshal(out, &b); err != nil {
			t.Fatalf("re-decode: %v\n%s", err, out)
		}
		b.Normalize()
		if err := b.Validate(); err != nil {
			t.Fatalf("round trip made a valid spec invalid: %v\n%s", err, out)
		}
		if a.Template() != b.Template() || a.Replicas != b.Replicas {
			t.Fatalf("round trip changed the spec:\n%s\n%s", a.Template(), b.Template())
		}
	})
}

// FuzzParseImage checks that a parsed reference re-parses to itself.
func FuzzParseImage(f *testing.F) {
	for _, s := range []string{"nginx", "a/b:c", "localhost:5000/x", "r.io/a@sha256:0000000000000000000000000000000000000000000000000000000000000000", "x:"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		img, err := ParseImage(s)
		if err != nil {
			return
		}
		again, err := ParseImage(img.String())
		if err != nil {
			t.Fatalf("%q normalised to %q, which does not parse: %v", s, img.String(), err)
		}
		if again != img {
			t.Fatalf("%q: %+v re-parsed as %+v", s, img, again)
		}
	})
}
