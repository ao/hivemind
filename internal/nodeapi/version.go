package nodeapi

import (
	"strings"

	"github.com/ao/hivemind/internal/store"
)

// VersionHeader carries a hivemind version, such as v2.2.0. A worker sends
// its own on every node API request, and the server stores it as the node's
// version. The server sends its own on every node API reply, and on every
// reply of the HTTP API to an authenticated request. Builds older than 2.2
// send none.
const VersionHeader = "Hivemind-Version"

// CleanVersion returns v when it can be stored and shown as a version: 1 to
// store.MaxNodeVersionLen letters, digits, dots, dashes, pluses and
// underscores. Anything else is "", which reads as no version.
func CleanVersion(v string) string {
	if v == "" || len(v) > store.MaxNodeVersionLen || strings.IndexFunc(v, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '-' || c == '+' || c == '_')
	}) >= 0 {
		return ""
	}
	return v
}
