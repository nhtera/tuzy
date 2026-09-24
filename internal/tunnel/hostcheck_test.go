package tunnel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nhtera/tuzy/internal/protocol"
)

func TestHostCheckRejectionSignatures(t *testing.T) {
	yes := []string{
		`Blocked request. This host ("nami-recording.tuzy.dev") is not allowed.
To allow this host, add "nami-recording.tuzy.dev" to ` + "`server.allowedHosts`" + ` in vite.config.js.`,
		"Invalid Host header",
		"<h1>Blocked hosts: shop.tuzy.dev</h1>",
		"Invalid HTTP_HOST header: 'shop.tuzy.dev'. You may need to add 'shop.tuzy.dev' to ALLOWED_HOSTS.",
	}
	for _, b := range yes {
		if !isHostCheckRejection([]byte(b)) {
			t.Errorf("not detected: %q", b)
		}
	}
	for _, b := range []string{"Forbidden", `{"error":"forbidden"}`, "403 Forbidden: missing token"} {
		if isHostCheckRejection([]byte(b)) {
			t.Errorf("false positive: %q", b)
		}
	}
}

// A Vite-like dev server that only accepts localhost Hosts.
func viteLike(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Host, "127.0.0.1") && !strings.HasPrefix(r.Host, "localhost") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `Blocked request. This host ("`+r.Host+`") is not allowed.`)
			return
		}
		_, _ = io.WriteString(w, "app "+r.Header.Get("X-Forwarded-Host"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func getVia(t *testing.T, ec *edgeConn) (int, string) {
	t.Helper()
	s := ec.open("GET", "/", "http", protocol.Header{"host", "nami-recording.tuzy.dev"}, protocol.Header{"x-forwarded-host", "nami-recording.tuzy.dev"})
	ec.send(protocol.Encode(protocol.ReqEnd, s.id, nil))
	head, body := s.response(t, 3*time.Second)
	return head.Status, string(body)
}

// Default (auto): the first rejection switches the tunnel to rewriting the Host and is replayed
// transparently; later requests are rewritten directly; the app still sees X-Forwarded-Host.
func TestAutoHostModeSwitchesAndRetries(t *testing.T) {
	vite := viteLike(t)
	edge := newFakeEdge(t)
	switches := make(chan Event, 4)
	startAgent(t, edge.url(), vite.URL, func(o *Options) {
		o.OnEvent = func(e Event) {
			if e.Kind == EventHostRewrite {
				switches <- e
			}
		}
	})
	ec := edge.next(3 * time.Second)
	for i := 0; i < 3; i++ {
		if status, body := getVia(t, ec); status != 200 || body != "app nami-recording.tuzy.dev" {
			t.Fatalf("request %d: %d %q", i, status, body)
		}
	}
	if len(switches) != 1 {
		t.Fatalf("switch events: %d, want exactly 1", len(switches))
	}
}

// Explicit modes are respected: preserve keeps the public Host (and flags the rejection so the
// CLI can explain it); rewrite never sees the rejection.
func TestExplicitHostModes(t *testing.T) {
	vite := viteLike(t)
	for _, tc := range []struct {
		mode    string
		status  int
		flagged bool
	}{{"preserve", 403, true}, {"rewrite", 200, false}} {
		t.Run(tc.mode, func(t *testing.T) {
			edge := newFakeEdge(t)
			entries := make(chan AccessEntry, 4)
			startAgent(t, edge.url(), vite.URL, func(o *Options) {
				o.HostHeader = tc.mode
				o.OnAccess = func(a AccessEntry) { entries <- a }
			})
			status, _ := getVia(t, edge.next(3*time.Second))
			if a := <-entries; status != tc.status || a.HostRejected != tc.flagged {
				t.Fatalf("status %d flagged %v", status, a.HostRejected)
			}
		})
	}
}
