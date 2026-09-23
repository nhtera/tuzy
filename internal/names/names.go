// Package names holds the tunnel-name rules shared by the CLI (mirrors edge/src/lib/host.ts).
package names

import (
	"regexp"
	"strings"
)

var labelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

// Valid reports whether name is a syntactically valid tunnel name: 3–32 chars of [a-z0-9-],
// alphanumeric at both ends, no "--" (blocks punycode). Reserved words are checked by the server.
func Valid(name string) bool {
	return labelRE.MatchString(name) && !strings.Contains(name, "--")
}
