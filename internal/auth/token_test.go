package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestNewTokenShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if !strings.HasPrefix(tok, TokenPrefix) {
			t.Fatalf("token %q lacks prefix %q", tok, TokenPrefix)
		}
		if len(tok) != tokenLen {
			t.Fatalf("token length %d, want %d", len(tok), tokenLen)
		}
		if err := ParseToken(tok); err != nil {
			t.Fatalf("ParseToken(own token): %v", err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q in 100 draws", tok)
		}
		seen[tok] = true
	}
}

func TestParseToken(t *testing.T) {
	valid, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"valid", valid, true},
		{"all-zero bytes", TokenPrefix + strings.Repeat("A", 43), true},
		{"empty", "", false},
		{"no prefix", valid[len(TokenPrefix):], false},
		{"wrong prefix", "xx_" + valid[len(TokenPrefix):], false},
		{"truncated", valid[:len(valid)-1], false},
		{"extended", valid + "A", false},
		{"standard base64 alphabet", TokenPrefix + strings.Repeat("+", 43), false},
		{"bad character", TokenPrefix + strings.Repeat("!", 43), false},
		{"padding", TokenPrefix + strings.Repeat("A", 42) + "=", false},
		{"newline inside", valid[:20] + "\n" + valid[21:], false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ParseToken(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("ParseToken(%q) = %v, want ok=%v", c.in, err, c.ok)
			}
		})
	}
}

func TestVerifyToken(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	want := HashToken(tok)
	cases := []struct {
		name      string
		presented string
		ok        bool
	}{
		{"right token", tok, true},
		{"wrong token", other, false},
		{"empty", "", false},
		{"prefix of right token", tok[:len(tok)-1], false},
		{"right token with suffix", tok + "x", false},
		{"digest bytes used as token", string(want[:]), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyToken(c.presented, want); got != c.ok {
				t.Fatalf("VerifyToken = %v, want %v", got, c.ok)
			}
		})
	}
}

// TestVerifyTokenUsesConstantTimeCompare checks the code structure, because
// timing cannot be tested reliably. VerifyToken must compare digests with
// subtle.ConstantTimeCompare and must not compare them with == or bytes.Equal.
func TestVerifyTokenUsesConstantTimeCompare(t *testing.T) {
	fn := findFunc(t, "token.go", "VerifyToken")
	var calls []string
	var eqOps int
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				calls = append(calls, sel.Sel.Name)
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "bytes" && sel.Sel.Name == "Equal" {
					t.Errorf("VerifyToken uses bytes.Equal, which is not constant-time for this use")
				}
			}
		case *ast.BinaryExpr:
			if x.Op == token.EQL || x.Op == token.NEQ {
				eqOps++
			}
		}
		return true
	})
	if !contains(calls, "ConstantTimeCompare") {
		t.Fatalf("VerifyToken does not call subtle.ConstantTimeCompare; calls: %v", calls)
	}
	if eqOps != 1 { // the == 1 on the ConstantTimeCompare result
		t.Fatalf("VerifyToken has %d ==/!= operators; want only the check on ConstantTimeCompare's result", eqOps)
	}
}

// findFunc parses one source file of this package and returns the named
// top-level function.
func findFunc(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("%s: no function %s", file, name)
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
