package tunnel

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nhtera/tuzy/internal/fileserver"
	"github.com/nhtera/tuzy/internal/upstream"
)

// TestFileTargetServesThroughTheTunnel is the phase-8 tunnel-level check: a file:// target (built
// the way internal/cli wires one up — FileTarget() + a fileserver.RoundTripper as LocalTransport)
// serves a real file to a visitor relayed through the fake edge.
func TestFileTargetServesThroughTheTunnel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello from disk"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := fileserver.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	edge := newFakeEdge(t)
	startAgent(t, edge.url(), "http://placeholder", func(o *Options) {
		o.Target = FileTarget()
		o.LocalTransport = rt
	})
	ec := edge.next(3 * time.Second)
	head, body := ec.get("/a.txt").response(t, 3*time.Second)
	if head.Status != 200 || string(body) != "hello from disk" {
		t.Fatalf("got %d %q", head.Status, body)
	}
}

// TestWSUpgradeToFileTargetIs501 checks the file target answers a websocket upgrade with 501,
// never attempting a dial.
func TestWSUpgradeToFileTargetIs501(t *testing.T) {
	dir := t.TempDir()
	rt, err := fileserver.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	edge := newFakeEdge(t)
	startAgent(t, edge.url(), "http://placeholder", func(o *Options) {
		o.Target = FileTarget()
		o.LocalTransport = rt
	})
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/socket", "ws")
	head, _ := s.response(t, 3*time.Second)
	if head.Status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", head.Status)
	}
}

// TestHTTPSUpstreamRequiresInsecureFlag mirrors the phase-8 requirement: a self-signed https
// upstream fails without --upstream-insecure and succeeds with it, relayed through the tunnel.
func TestHTTPSUpstreamRequiresInsecureFlag(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secure hello"))
	}))
	defer app.Close()
	edge := newFakeEdge(t)

	startAgent(t, edge.url(), app.URL, func(o *Options) {
		o.LocalTransport = upstream.NewHTTPSTransport(upstream.TLSOptions{})
	})
	ec := edge.next(3 * time.Second)
	head, _ := ec.get("/").response(t, 3*time.Second)
	if head.Status != http.StatusBadGateway {
		t.Fatalf("strict transport status = %d, want 502 (cert should be rejected)", head.Status)
	}

	edge2 := newFakeEdge(t)
	startAgent(t, edge2.url(), app.URL, func(o *Options) {
		o.LocalTransport = upstream.NewHTTPSTransport(upstream.TLSOptions{Insecure: true})
	})
	ec2 := edge2.next(3 * time.Second)
	head2, body2 := ec2.get("/").response(t, 3*time.Second)
	if head2.Status != 200 || string(body2) != "secure hello" {
		t.Fatalf("insecure transport: status=%d body=%q", head2.Status, body2)
	}
}

// TestWSThroughHTTPSUpstream checks a websocket upgrade relays correctly to an https upstream
// (ws_stream.go dials wss:// and uses the tunnel's LocalTransport for the TLS handshake).
func TestWSThroughHTTPSUpstream(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not a websocket handler, but reachable", http.StatusUpgradeRequired)
	}))
	defer app.Close()
	edge := newFakeEdge(t)
	startAgent(t, edge.url(), app.URL, func(o *Options) {
		o.LocalTransport = upstream.NewHTTPSTransport(upstream.TLSOptions{Insecure: true})
	})
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/socket", "ws")
	head, _ := s.response(t, 3*time.Second)
	// The TLS handshake and HTTP round trip succeeded (reached the app, which then refused the
	// upgrade): proves the wss:// dial went through the https upstream's own transport, not a
	// plaintext one.
	if head.Status != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want %d (app reached over TLS)", head.Status, http.StatusUpgradeRequired)
	}
}
