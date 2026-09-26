package tunnel

import (
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"strings"

	"github.com/nhtera/tuzy/internal/protocol"
)

//go:embed local_error_page.html
var localErrorPageHTML string

var localErrorPage = template.Must(template.New("local-error").Parse(localErrorPageHTML))

// localErrorCSP locks the agent's own error page down to its inline styles and data: favicon.
const localErrorCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// unreachableResponse is the 502 the agent answers with when the local target can't be reached.
// Browsers (Accept: text/html) get a page showing where the request stopped; everything else
// (curl, API clients, WebSocket handshakes) keeps the one-line text answer. The target is shown
// with any password redacted; the error detail is shown publicly so the owner can debug.
func unreachableResponse(reqHeaders []protocol.Header, target *url.URL, err error) ([]protocol.Header, string) {
	if !acceptsHTML(reqHeaders) {
		return plainText(), fmt.Sprintf("tuzy: could not reach %s\n", target.Redacted())
	}
	var b strings.Builder
	_ = localErrorPage.Execute(&b, struct{ Target, Detail string }{target.Redacted(), dialDetail(err)})
	return []protocol.Header{
		{"content-type", "text/html; charset=utf-8"},
		{"cache-control", "no-store"},
		{"content-security-policy", localErrorCSP},
		{"x-content-type-options", "nosniff"},
	}, b.String()
}

// dialDetail is the underlying error (e.g. "dial tcp [::1]:3000: connect: connection refused").
// The HTTP path's RoundTrip already returns it bare; the WebSocket dial wraps it in a *url.Error
// that would repeat the visitor's full request URL, so that wrapper is dropped.
func dialDetail(err error) string {
	if err == nil {
		return "unknown error"
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err.Error()
	}
	return err.Error()
}

func acceptsHTML(headers []protocol.Header) bool {
	for _, kv := range headers {
		if strings.EqualFold(kv[0], "accept") && strings.Contains(strings.ToLower(kv[1]), "text/html") {
			return true
		}
	}
	return false
}
