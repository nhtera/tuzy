package tunnel

import "bytes"

// Dev servers that reject unknown Host headers answer 400/403 with one of these bodies. With
// the public Host (<name>.tuzy.dev) the agent switches auto mode to rewriting (and replays the
// request), or, with an explicit --host-header preserve, flags the entry so the CLI explains it.
var hostCheckSignatures = [][]byte{
	[]byte("Blocked request. This host"), // Vite (server.allowedHosts)
	[]byte("Invalid Host header"),        // webpack-dev-server / Create React App / Angular CLI
	[]byte("Blocked hosts:"),             // Rails (config.hosts)
	[]byte("Invalid HTTP_HOST header"),   // Django (ALLOWED_HOSTS)
	[]byte("DisallowedHost"),             // Django debug page
}

// hostCheckSniffLimit is how much of a 400/403 body is inspected.
const hostCheckSniffLimit = 2048

func isHostCheckRejection(body []byte) bool {
	for _, sig := range hostCheckSignatures {
		if bytes.Contains(body, sig) {
			return true
		}
	}
	return false
}
