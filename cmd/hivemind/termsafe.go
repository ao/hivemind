package main

import (
	"strconv"
	"strings"
	"unicode"
)

// termSafe writes each control character in s as Go's %q does (\n, \x1b,
// \u009b), so that text from the server, which can come from a worker,
// cannot move the cursor, change colours or clear the screen of the
// terminal it is printed on. status, nodes, node drain and the server's
// error messages print through it. logs does not: it copies a container's
// output as it is, as docker logs does.
func termSafe(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteRune(r)
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}
