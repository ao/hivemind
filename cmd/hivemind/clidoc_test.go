package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIDocIsCurrent fails when docs/cli.md no longer matches the command
// definitions. Run `make docs` to regenerate it.
func TestCLIDocIsCurrent(t *testing.T) {
	want := renderCLIDoc()
	path := filepath.Join("..", "..", filepath.FromSlash(cliDocPath))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (run `make docs`)", cliDocPath, err)
	}
	if bytes.Equal(got, []byte(want)) {
		return
	}
	gotLines := strings.Split(string(got), "\n")
	wantLines := strings.Split(want, "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("%s is stale: first difference at line %d\n  file:      %q\n  generated: %q\nrun `make docs` and commit the result", cliDocPath, i+1, g, w)
		}
	}
	t.Fatalf("%s is stale: the line counts differ; run `make docs` and commit the result", cliDocPath)
}

// TestCLIDocOmitsHiddenCommands checks that the hidden generator does not
// appear in the reference, and that every visible command does.
func TestCLIDocOmitsHiddenCommands(t *testing.T) {
	doc := renderCLIDoc()
	if strings.Contains(doc, "gen-docs") {
		t.Fatal("the reference lists the hidden gen-docs command")
	}
	for _, c := range visibleCommands(newRoot(strings.NewReader(""), new(bytes.Buffer), new(bytes.Buffer))) {
		if !strings.Contains(doc, "## hivemind "+c.Name()+"\n") {
			t.Errorf("the reference has no section for %q", c.Name())
		}
	}
}

// TestCLIDocDataDirDefaultIsStable checks that the reference does not print
// the default data directory of the machine that generated it.
func TestCLIDocDataDirDefaultIsStable(t *testing.T) {
	doc := renderCLIDoc()
	if home := os.Getenv("HOME"); len(home) > 1 && strings.Contains(doc, home) {
		t.Fatal("the reference contains the HOME directory of the machine that generated it")
	}
	if !strings.Contains(doc, "The default is /var/lib/hivemind for root") {
		t.Fatal("the data-dir rule is missing from the reference")
	}
}
