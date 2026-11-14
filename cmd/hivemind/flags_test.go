package main

import (
	"strings"
	"testing"
)

// Commands do not take flags they would ignore. serve, init and join run a
// node and never call the API, so they have no --server, and only join has
// a --token, which is its join token. logs prints the container's own text,
// so it has no --json.
func TestNoIgnoredFlags(t *testing.T) {
	isolate(t)
	for _, c := range []struct {
		args []string
		flag string
	}{
		{[]string{"init", "--server", "http://127.0.0.1:4483"}, "--server"},
		{[]string{"init", "--token", "hm_x"}, "--token"},
		{[]string{"serve", "--server", "http://127.0.0.1:4483"}, "--server"},
		{[]string{"serve", "--token", "hm_x"}, "--token"},
		{[]string{"join", "10.0.0.5", "--server", "http://127.0.0.1:4483", "--token", "t"}, "--server"},
		{[]string{"logs", "web", "--json"}, "--json"},
	} {
		_, _, err := run(t, "", c.args...)
		if err == nil || !strings.Contains(err.Error(), "unknown flag: "+c.flag) {
			t.Errorf("%v: %v, want unknown flag %s", c.args, err, c.flag)
		}
	}

	// The commands that do use them still have them.
	root := newRoot(strings.NewReader(""), new(strings.Builder), new(strings.Builder))
	for _, c := range []struct{ cmd, flag string }{
		{"join", "token"}, {"logs", "server"}, {"logs", "token"}, {"status", "json"}, {"nodes", "server"},
	} {
		sub, _, err := root.Find([]string{c.cmd})
		if err != nil || sub.Flags().Lookup(c.flag) == nil {
			t.Errorf("%s has no --%s: %v", c.cmd, c.flag, err)
		}
	}
	join, _, _ := root.Find([]string{"join"})
	if f := join.Flags().Lookup("token"); f == nil || !strings.Contains(f.Usage, "join token") {
		t.Errorf("join --token: %+v", f)
	}
}
