package tunnel

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/url"
	"strings"
	"syscall"

	"github.com/nhtera/tuzy/internal/protocol"
)

//go:embed local_error_page.html
var localErrorPageHTML string

var localErrorPage = template.Must(template.New("local-error").Parse(localErrorPageHTML))

// localErrorCSP locks the agent's own error page down to its inline styles and data: favicon.
const localErrorCSP = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// localUnreachableCode is this page's stable error code: sent as the tuzy-error header (not
// x-tuzy-*, which the edge strips from agent responses) and documented under this anchor.
const (
	localUnreachableCode = "TUZY-502-LOCAL-UNREACHABLE"
	localUnreachableDocs = "https://github.com/nhtera/tuzy/blob/main/docs/limits-and-faq.md#tuzy-502-local-unreachable"
)

// unreachableResponse is the 502 the agent answers with when the local target can't be reached.
// Browsers (Accept: text/html) get a page showing where the request stopped; everything else
// (curl, API clients, WebSocket handshakes) keeps the one-line text answer. The target is shown
// with any password redacted; the error detail is shown publicly so the owner can debug.
func unreachableResponse(reqHeaders []protocol.Header, target *url.URL, err error) ([]protocol.Header, string) {
	if !acceptsHTML(reqHeaders) {
		return append(plainText(), protocol.Header{"tuzy-error", localUnreachableCode}),
			fmt.Sprintf("tuzy: could not reach %s (%s)\n", target.Redacted(), localUnreachableCode)
	}
	var b strings.Builder
	_ = localErrorPage.Execute(&b, struct{ Target, Detail, Kind, Code, Docs string }{
		target.Redacted(), dialDetail(err), dialKind(err), localUnreachableCode, localUnreachableDocs,
	})
	return []protocol.Header{
		{"content-type", "text/html; charset=utf-8"},
		{"cache-control", "no-store"},
		{"content-security-policy", localErrorCSP},
		{"x-content-type-options", "nosniff"},
		{"tuzy-error", localUnreachableCode},
	}, b.String()
}

// wsaETimedOut is Windows' connect timeout (WSAETIMEDOUT), which its Errno.Timeout() doesn't report.
const wsaETimedOut = syscall.Errno(10060)

// dialKind picks the page's fix-it hint: "dns", "tls" or "timeout"; "" for everything else
// (above all a refused connection, whose wording differs per OS, so it is the default hint).
func dialKind(err error) string {
	var dnsErr *net.DNSError
	var recErr tls.RecordHeaderError
	var verifyErr *tls.CertificateVerificationError
	var authErr x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var invalidErr x509.CertificateInvalidError
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &recErr), errors.As(err, &verifyErr), errors.As(err, &authErr),
		errors.As(err, &hostErr), errors.As(err, &invalidErr):
		return "tls"
	case errors.As(err, &netErr) && netErr.Timeout(), errors.Is(err, wsaETimedOut):
		return "timeout"
	}
	return ""
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
