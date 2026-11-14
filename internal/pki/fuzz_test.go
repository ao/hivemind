package pki

import "testing"

// FuzzParseJoinToken must not panic. A string that parses must be the exact
// text of the token it decodes to.
func FuzzParseJoinToken(f *testing.F) {
	valid := JoinToken{Fingerprint: [32]byte{1, 2, 3}, Secret: [32]byte{9, 8, 7}}.String()
	f.Add(valid)
	f.Add(tokenPrefix + "0000000000000000000000000000000000000000000000000000000000000000.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	f.Add("hivemind-join-v1..")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		tok, err := ParseJoinToken(s)
		if err != nil {
			return
		}
		if got := tok.String(); got != s {
			t.Fatalf("%q parsed, then formatted as %q", s, got)
		}
	})
}

// FuzzParseNodeURI must not panic. A URI that parses must rebuild exactly.
func FuzzParseNodeURI(f *testing.F) {
	f.Add("hivemind://node/n1")
	f.Add("hivemind://node/a-b-c")
	f.Add("hivemind://node/")
	f.Add("hivemind://node/Bad")
	f.Add("hivemind://node/a?x=1")
	f.Add("hivemind://node/a/../b")
	f.Fuzz(func(t *testing.T, s string) {
		id, err := ParseNodeURI(s)
		if err != nil {
			return
		}
		back, err := NodeURI(id)
		if err != nil {
			t.Fatalf("%q parsed to ID %q, which NodeURI rejects: %v", s, id, err)
		}
		if back != s {
			t.Fatalf("%q parsed to %q, which rebuilds as %q", s, id, back)
		}
	})
}
