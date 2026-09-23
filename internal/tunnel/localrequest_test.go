package tunnel

// TestBuildLocalRequest* is M6: the live path (http_stream.go) and the inspector's replay share
// BuildLocalRequest, so the hop-by-hop header list, Content-Length/NoBody-by-method handling and
// the Fragment reset stay byte-identical between them. Exercised directly here at the unit level;
// the live path's own behaviour is still covered end-to-end by review_test.go and tunnel_test.go.

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/nhtera/tuzy/internal/protocol"
)

// A visitor's request-URI never carries a fragment (RFC 7230: browsers strip it before sending),
// but a locally configured target URL could ("http://localhost:3000/base#stale") — targetURL
// resets it defensively so it can never leak into the outgoing request.
func TestBuildLocalRequestFragmentReset(t *testing.T) {
	target, _ := url.Parse("http://localhost:3000/base#stale-fragment")
	req, err := BuildLocalRequest(context.Background(), target, "preserve", "GET", "/a?x=1", nil, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Fragment != "" {
		t.Fatalf("Fragment not reset: %q", req.URL.Fragment)
	}
	if req.URL.RawQuery != "x=1" {
		t.Fatalf("query lost: %q", req.URL.RawQuery)
	}
}

func TestBuildLocalRequestHopByHopAndHostHeader(t *testing.T) {
	target, _ := url.Parse("http://localhost:3000")
	headers := []protocol.Header{
		{"host", "visitor.example"},
		{"connection", "keep-alive"},
		{"x-kept", "yes"},
	}
	req, err := BuildLocalRequest(context.Background(), target, "preserve", "GET", "/p", headers, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if req.Host != "visitor.example" {
		t.Fatalf("preserve mode: Host = %q", req.Host)
	}
	if req.Header.Get("Connection") != "" {
		t.Fatalf("hop-by-hop header forwarded: %v", req.Header)
	}
	if req.Header.Get("X-Kept") != "yes" {
		t.Fatalf("ordinary header dropped: %v", req.Header)
	}

	req2, err := BuildLocalRequest(context.Background(), target, "rewrite", "GET", "/p", headers, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if req2.Host == "visitor.example" {
		t.Fatal("rewrite mode must use the local target's host, not the visitor's")
	}
}

func TestBuildLocalRequestBodyLenAndMethod(t *testing.T) {
	target, _ := url.Parse("http://localhost:3000")

	// bodyLen known (replay: len(body)) always wins, even with a stale Content-Length header.
	req, err := BuildLocalRequest(context.Background(), target, "preserve", "POST", "/p",
		[]protocol.Header{{"content-length", "999"}}, strings.NewReader("abc"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != 3 {
		t.Fatalf("ContentLength = %d, want 3 (the known body length, not the stale header)", req.ContentLength)
	}

	// bodyLen unknown (-1, live path): a GET with no declared length and no body gets NoBody.
	req2, err := BuildLocalRequest(context.Background(), target, "preserve", "GET", "/p", nil, nil, -1)
	if err != nil {
		t.Fatal(err)
	}
	if req2.Body != nil && req2.ContentLength != 0 {
		t.Fatalf("bodyless GET should have ContentLength 0, got %d", req2.ContentLength)
	}

	// bodyLen unknown, method that may have a body, no declared length: streamed (-1).
	req3, err := BuildLocalRequest(context.Background(), target, "preserve", "POST", "/p", nil, strings.NewReader(""), -1)
	if err != nil {
		t.Fatal(err)
	}
	if req3.ContentLength != -1 {
		t.Fatalf("expected an unknown (streamed) length for POST with no Content-Length, got %d", req3.ContentLength)
	}
}
