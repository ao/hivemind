package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

// FuzzDecodeApp feeds arbitrary bodies to the API's app-spec decoding path.
// It must never panic. A body it accepts must be a valid spec that survives
// a JSON round trip through the same path unchanged, and a body it rejects
// must carry a 400 or 413 status.
func FuzzDecodeApp(f *testing.F) {
	for _, s := range []string{
		`{"name":"web","image":"nginx","replicas":2,"ports":[{"host":8080,"container":80}]}`,
		`{"name":"db","image":"postgres:16","volumes":[{"name":"pg","path":"/var/lib/postgresql/data"}],"resources":{"memory":"1Gi","cpu":0.5}}`,
		`{"name":"x","image":"a/b:c","health":{"http":{"port":1,"path":"/h"},"interval":"1s","timeout":"1s"}}`,
		`{"name":"x","image":"a","env":{"A":"b"},"command":["sh","-c","echo"]}`,
		`{"name":"x","image":"a","unknown":1}`,
		`{"name":"x","image":"a"} trailing`,
		`{"replicas":1e400}`,
		`[]`, `null`, ``, `{`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := DecodeApp(bytes.NewReader(data))
		if err != nil {
			var re *requestError
			if !errors.As(err, &re) || (re.code != http.StatusBadRequest && re.code != http.StatusRequestEntityTooLarge) {
				t.Fatalf("rejection without a 400 or 413: %v", err)
			}
			return
		}
		if err := a.Validate(); err != nil {
			t.Fatalf("accepted an invalid spec: %v", err)
		}
		out, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal accepted spec: %v", err)
		}
		b, err := DecodeApp(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("re-decoding an accepted spec failed: %v\n%s", err, out)
		}
		if a.Template() != b.Template() || a.Replicas != b.Replicas || a.Name != b.Name {
			t.Fatalf("round trip changed the spec:\n%s\n%s", a.Template(), b.Template())
		}
	})
}
