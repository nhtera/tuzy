package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// The WebSocket dial wraps the dial error in a *url.Error carrying the visitor's URL; the page
// must show only the dial error.
func TestDialDetailDropsVisitorURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "http://127.0.0.1:1/secret?q=<script>", nil)
	if err == nil {
		t.Fatal("dial to port 1 succeeded")
	}
	d := dialDetail(err)
	if strings.Contains(d, "/secret") || !strings.Contains(d, "127.0.0.1:1") {
		t.Fatalf("detail %q (from %v)", d, err)
	}
}

func TestUnreachableResponseRedactsPassword(t *testing.T) {
	u, _ := url.Parse("http://user:hunter2@localhost:3000")
	for _, accept := range []string{"text/html", "*/*"} {
		_, body := unreachableResponse([]protocol.Header{{"accept", accept}}, u, nil)
		if strings.Contains(body, "hunter2") {
			t.Errorf("accept %q: password in body", accept)
		}
	}
}

func TestUnreachableResponseCarriesCode(t *testing.T) {
	u, _ := url.Parse("http://localhost:3000")
	for _, accept := range []string{"text/html", "*/*"} {
		headers, body := unreachableResponse([]protocol.Header{{"accept", accept}}, u, errors.New("boom"))
		var code string
		for _, h := range headers {
			if h[0] == "tuzy-error" {
				code = h[1]
			}
		}
		if code != "TUZY-502-LOCAL-UNREACHABLE" || !strings.Contains(body, code) {
			t.Errorf("accept %q: header %q, body %q", accept, code, body)
		}
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestDialKind(t *testing.T) {
	op := func(err error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: err} }
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"refused", op(syscall.ECONNREFUSED), ""},
		{"other", errors.New("x"), ""},
		{"dns", op(&net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}), "dns"},
		{"dns timeout is still dns", op(&net.DNSError{Err: "timeout", IsTimeout: true}), "dns"},
		{"unknown authority", fmt.Errorf("wrapped: %w", x509.UnknownAuthorityError{}), "tls"},
		{"hostname", &tls.CertificateVerificationError{Err: x509.HostnameError{Host: "x"}}, "tls"},
		{"timeout", op(timeoutErr{}), "timeout"},
		{"visitor url wrapper", &url.Error{Op: "Get", URL: "http://x/", Err: op(timeoutErr{})}, "timeout"},
	} {
		if got := dialKind(tc.err); got != tc.want {
			t.Errorf("%s: dialKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// An https:// target that speaks plain HTTP fails the handshake with a RecordHeaderError: the
// page must suggest an http:// target, not the default "is it running?" hint.
func TestDialKindTLSToPlainHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, err := (&http.Transport{}).RoundTrip(httptest.NewRequest("GET", strings.Replace(srv.URL, "http://", "https://", 1), nil))
	if err == nil {
		t.Fatal("TLS to a plain HTTP server succeeded")
	}
	if got := dialKind(err); got != "tls" {
		t.Fatalf("dialKind(%v) = %q", err, got)
	}
}

// Every error code the agent and the edge send has a heading in docs/limits-and-faq.md, so the
// pages' "What does this mean?" link lands on it.
func TestErrorCodesDocumented(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	docs := read("../../docs/limits-and-faq.md")
	codes := regexp.MustCompile(`TUZY-\d{3}-[A-Z-]*[A-Z]`)
	found := codes.FindAllString(read("../../edge/src/pages/status-pages.ts"), -1)
	found = append(found, localUnreachableCode)
	if len(found) < 10 {
		t.Fatalf("only %d codes found in status-pages.ts", len(found))
	}
	for _, code := range found {
		if !strings.Contains(docs, "\n### "+code+"\n") {
			t.Errorf("docs/limits-and-faq.md has no heading for %s", code)
		}
	}
	if !strings.HasSuffix(localUnreachableDocs, "#"+strings.ToLower(localUnreachableCode)) {
		t.Errorf("docs link %s does not point at %s", localUnreachableDocs, localUnreachableCode)
	}
}
