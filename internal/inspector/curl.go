package inspector

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

// shellQuote POSIX-single-quotes s ('…' with ' → '\”).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// skipCurlHeader lists headers curl sets itself or the edge adds.
var skipCurlHeader = map[string]bool{
	"host": true, "content-length": true, "connection": true, "transfer-encoding": true,
	"x-forwarded-for": true, "x-forwarded-proto": true, "x-forwarded-host": true, "x-real-ip": true,
}

// curlCommand reproduces an entry against baseURL (the tunnel's public URL).
//
//   - Text bodies use --data-raw, never --data-binary: --data-binary treats an argument starting
//     with '@' as a filename to read (regardless of quoting), so a captured body that happens to
//     start with '@' would silently not be sent as-is. --data-raw has no such special case (M3).
//   - Binary (non-UTF-8) bodies are piped through base64 on stdin (--data-binary @-): '@-' means
//     stdin, not a file, so it is always safe.
//   - HEAD uses curl's -I rather than -X HEAD (M3).
//   - A truncated/evicted/incomplete body gets a leading comment: the command would only replay a
//     partial body.
func curlCommand(e *Entry, baseURL string) string {
	var b strings.Builder
	body := e.ReqBody
	if len(body) > 0 && (e.ReqTrunc || e.Evicted || e.ReqIncomplete) {
		b.WriteString("# body truncated in the inspector: this replays only the captured prefix.\n")
	}
	binary := len(body) > 0 && (!utf8.Valid(body) || strings.ContainsRune(string(body), 0))
	if binary {
		b.WriteString("printf %s " + shellQuote(base64.StdEncoding.EncodeToString(body)) + " | base64 -d | ")
	}
	method := strings.ToUpper(e.Method)
	target := shellQuote(strings.TrimRight(baseURL, "/") + e.Path)
	if method == "HEAD" {
		b.WriteString("curl -I " + target)
	} else {
		b.WriteString("curl -X " + shellQuote(e.Method) + " " + target)
	}
	for _, h := range e.ReqHeaders {
		if skipCurlHeader[strings.ToLower(h[0])] {
			continue
		}
		b.WriteString(" \\\n  -H " + shellQuote(h[0]+": "+h[1]))
	}
	switch {
	case method == "HEAD":
		// -I never sends a body.
	case binary:
		b.WriteString(" \\\n  --data-binary @-")
	case len(body) > 0:
		b.WriteString(" \\\n  --data-raw " + shellQuote(string(body)))
	}
	return b.String()
}
