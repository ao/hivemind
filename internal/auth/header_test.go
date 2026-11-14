package auth

import (
	"strings"
	"testing"
)

func TestParseBearer(t *testing.T) {
	cases := []struct {
		name   string
		header string
		token  string
		ok     bool
	}{
		{"canonical", "Bearer hm_abc", "hm_abc", true},
		{"lower scheme", "bearer hm_abc", "hm_abc", true},
		{"upper scheme", "BEARER hm_abc", "hm_abc", true},
		{"mixed scheme", "bEaReR hm_abc", "hm_abc", true},
		{"empty", "", "", false},
		{"scheme only", "Bearer", "", false},
		{"scheme and space", "Bearer ", "", false},
		{"two spaces", "Bearer  hm_abc", "", false},
		{"leading space", " Bearer hm_abc", "", false},
		{"trailing space", "Bearer hm_abc ", "", false},
		{"tab separator", "Bearer\thm_abc", "", false},
		{"other scheme", "Basic dXNlcjpwYXNz", "", false},
		{"token without scheme", "hm_abc", "", false},
		{"two fields", "Bearer hm_abc extra", "", false},
		{"control character", "Bearer hm_\x00abc", "", false},
		{"newline", "Bearer hm_abc\r\nX-Injected: 1", "", false},
		{"unicode", "Bearer hm_é", "", false},
		{"b64token characters", "Bearer a-._~+/==", "a-._~+/==", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseBearer(c.header)
			if ok != c.ok || got != c.token {
				t.Fatalf("ParseBearer(%q) = (%q, %v), want (%q, %v)", c.header, got, ok, c.token, c.ok)
			}
		})
	}
}

// FuzzParseBearer checks that the parser never panics, that any token it
// returns is a non-empty b64token, and that the token survives a round trip.
func FuzzParseBearer(f *testing.F) {
	for _, s := range []string{
		"Bearer hm_abc",
		"bearer x",
		"Bearer",
		"Bearer  x",
		"Basic dXNlcjpwYXNz",
		"Bearer \x00",
		"Bearer a=b=c",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, header string) {
		token, ok := ParseBearer(header)
		if !ok {
			if token != "" {
				t.Fatalf("%q: returned token %q with ok=false", header, token)
			}
			return
		}
		if token == "" {
			t.Fatalf("%q: ok with empty token", header)
		}
		for i := 0; i < len(token); i++ {
			if !isB64Token(token[i]) {
				t.Fatalf("%q: token %q has byte %#x outside b64token", header, token, token[i])
			}
		}
		again, ok2 := ParseBearer("Bearer " + token)
		if !ok2 || again != token {
			t.Fatalf("%q: token %q did not round trip (got %q, %v)", header, token, again, ok2)
		}
		if !strings.EqualFold(header[:min(len(header), len("bearer "))], "bearer ") {
			t.Fatalf("%q: accepted without the Bearer scheme", header)
		}
	})
}
