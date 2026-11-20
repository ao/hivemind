package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The store is replicated by recording the write statements of each
// transaction and running them again on every server (docs/ha.md). That
// holds only if every write is in a transaction of Store.tx, and if no
// statement makes a value of its own. These tests read the package's
// source for both.

// storeSources returns the package's Go files that are not tests, by name.
func storeSources(t *testing.T) map[string]string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		out[n] = string(b)
	}
	if len(out) < 10 {
		t.Fatalf("read %d source files; run the test from the package directory", len(out))
	}
	return out
}

// TestWritesAreInTransactions fails on a statement run on the write
// connection outside a transaction. The one allowed is VACUUM INTO, which
// writes another file.
func TestWritesAreInTransactions(t *testing.T) {
	re := regexp.MustCompile(`\b(s\.db|db)\.(ExecContext|Exec|PrepareContext|Prepare)\(`)
	for name, src := range storeSources(t) {
		for i, line := range strings.Split(src, "\n") {
			if !re.MatchString(line) {
				continue
			}
			if strings.Contains(line, "VACUUM INTO") || strings.Contains(line, "SELECT total_changes()") || name == "schema.go" || name == "cluster.go" && strings.Contains(line, "BeginTx") {
				continue
			}
			t.Errorf("%s:%d runs a statement outside Store.tx: %s", name, i+1, strings.TrimSpace(line))
		}
	}
}

// TestNoValuesMadeInSQL fails on a function that makes a value of its own
// in the store's SQL: a time, a random value, or a write that hands rows
// back. Each server would make a different one when it applies the entry.
func TestNoValuesMadeInSQL(t *testing.T) {
	re := regexp.MustCompile(`(?i)\b(random|randomblob|datetime|julianday|strftime|unixepoch|date|time|current_timestamp|current_date|current_time|last_insert_rowid|changes)\s*\(|\bcurrent_(timestamp|date|time)\b|\breturning\b|\bautoincrement\b|\bcreate\s+(temp|temporary)\b|\bcreate\s+trigger\b`)
	for name, src := range storeSources(t) {
		if name == "recdriver.go" {
			continue // its literals are the patterns it refuses, not SQL
		}
		for _, lit := range sqlLiterals(src) {
			if m := re.FindString(lit.text); m != "" {
				// total_changes() is read on the leader only, to see whether
				// a transaction changed anything; it is never written.
				if strings.EqualFold(strings.TrimSpace(m), "changes(") && strings.Contains(lit.text, "total_changes()") {
					continue
				}
				t.Errorf("%s:%d: SQL uses %q, which a replay would not repeat: %s", name, lit.line, m, firstLine(lit.text))
			}
		}
	}
}

type literal struct {
	line int
	text string
}

// sqlLiterals returns the back-quoted strings of src, which is where the
// store's SQL is.
func sqlLiterals(src string) []literal {
	var out []literal
	line := 1
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '\n':
			line++
		case '`':
			j := strings.IndexByte(src[i+1:], '`')
			if j < 0 {
				return out
			}
			text := src[i+1 : i+1+j]
			out = append(out, literal{line: line, text: text})
			line += strings.Count(text, "\n")
			i += j + 1
		case '"':
			// Skip a quoted string, which may hold a back quote.
			for i++; i < len(src) && src[i] != '"' && src[i] != '\n'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '\'':
			for i++; i < len(src) && src[i] != '\'' && src[i] != '\n'; i++ {
				if src[i] == '\\' {
					i++
				}
			}
		case '/':
			if i+1 < len(src) && src[i+1] == '/' {
				for i < len(src) && src[i] != '\n' {
					i++
				}
				line++
			}
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " ..."
	}
	return s
}
