package diagnose

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type edge struct {
	health string // body for /api/v1/health ("" = healthy)
	noWS   bool
	me     int // status for /api/v1/me
}

func fakeEdge(t *testing.T, e edge) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/health":
			if e.health != "" {
				_, _ = w.Write([]byte(e.health))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": "v-test", "min_proto": 1, "max_proto": 1})
		case "/api/v1/diagnose/ws":
			if e.noWS {
				http.Error(w, "blocked by proxy", http.StatusForbidden)
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.CloseNow() }()
			for {
				typ, b, err := c.Read(r.Context())
				if err != nil {
					return
				}
				_ = c.Write(r.Context(), typ, b)
			}
		case "/api/v1/me":
			if e.me != http.StatusOK {
				w.WriteHeader(e.me)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"not logged in: run ` + "`tuzy login`" + `"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"user":{"email":"a@b.c"},"token":{"scope":"full","label":"mbp"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func run(t *testing.T, srv *httptest.Server, pool *x509.CertPool, mod func(*Config)) map[string]Result {
	t.Helper()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	u, _ := url.Parse(srv.URL)
	cfg := Config{Server: u, Token: "tzy_x", UserAgent: "tuzy/test", Proto: 1, RootCAs: pool, Timeout: 3 * time.Second, InspectAddr: "127.0.0.1:0"}
	if mod != nil {
		mod(&cfg)
	}
	out := map[string]Result{}
	for _, r := range Run(context.Background(), cfg) {
		out[r.Name] = r
	}
	return out
}

func want(t *testing.T, rs map[string]Result, name string, s Status) {
	t.Helper()
	if rs[name].Status != s {
		t.Fatalf("%s = %+v, want %s", name, rs[name], s)
	}
}

func TestAllGreen(t *testing.T) {
	srv, pool := fakeEdge(t, edge{me: 200})
	rs := run(t, srv, pool, nil)
	for _, n := range []string{"proxy", "dns", "tls", "api", "clock", "websocket", "token", "inspector"} {
		want(t, rs, n, OK)
	}
	if Failed(mapToSlice(rs)) {
		t.Fatal("unexpected failure")
	}
}

func TestFailureModes(t *testing.T) {
	t.Run("tls interception (unknown authority)", func(t *testing.T) {
		srv, _ := fakeEdge(t, edge{me: 200})
		rs := run(t, srv, x509.NewCertPool(), nil)
		want(t, rs, "tls", Fail)
		if rs["tls"].Hint == "" {
			t.Fatal("missing hint")
		}
	})
	t.Run("websocket blocked", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{noWS: true, me: 200})
		rs := run(t, srv, pool, nil)
		want(t, rs, "websocket", Fail)
		want(t, rs, "api", OK)
	})
	t.Run("bad token", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{me: 401})
		want(t, run(t, srv, pool, nil), "token", Fail)
	})
	t.Run("not logged in is a warning", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{me: 200})
		want(t, run(t, srv, pool, func(c *Config) { c.Token = "" }), "token", Warn)
	})
	t.Run("clock skew", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{me: 200})
		want(t, run(t, srv, pool, func(c *Config) { c.Now = func() time.Time { return time.Now().Add(10 * time.Minute) } }), "clock", Fail)
		want(t, run(t, srv, pool, func(c *Config) { c.Now = func() time.Time { return time.Now().Add(-2 * time.Minute) } }), "clock", Warn)
	})
	t.Run("captive portal answers instead of the API", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{health: "<html>login to wifi</html>", me: 200})
		want(t, run(t, srv, pool, nil), "api", Fail)
	})
	t.Run("protocol mismatch", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{me: 200})
		want(t, run(t, srv, pool, func(c *Config) { c.Proto = 2 }), "api", Fail)
	})
	t.Run("no internet / DNS", func(t *testing.T) {
		u, _ := url.Parse("https://does-not-exist.invalid")
		rs := map[string]Result{}
		for _, r := range Run(context.Background(), Config{Server: u, Timeout: 2 * time.Second, InspectAddr: "127.0.0.1:0"}) {
			rs[r.Name] = r
		}
		want(t, rs, "dns", Fail)
		want(t, rs, "api", Fail)
	})
	t.Run("inspector port busy", func(t *testing.T) {
		srv, pool := fakeEdge(t, edge{me: 200})
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		want(t, run(t, srv, pool, func(c *Config) { c.InspectAddr = ln.Addr().String() }), "inspector", Warn)
	})
}

func TestProxyCredentialsAreNotPrinted(t *testing.T) {
	p, _ := url.Parse("http://user:secret@proxy.example:3128")
	r := checkProxy(p)
	if r.Detail != "using http://proxy.example:3128 (from HTTPS_PROXY/HTTP_PROXY)" {
		t.Fatalf("detail = %q", r.Detail)
	}
}

func mapToSlice(m map[string]Result) []Result {
	out := make([]Result, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	return out
}

func TestTLSClassificationForProxiedConnections(t *testing.T) {
	// Behind a proxy the TLS verdict comes from the proxied health request (a direct dial would
	// bypass the proxy). Go never proxies loopback, so the helpers are tested directly.
	r, ok := tlsHint(x509.UnknownAuthorityError{}, "tuzy.dev")
	if !ok || r.Status != Fail || !strings.Contains(r.Hint, "TLS-intercepting") {
		t.Fatalf("%+v", r)
	}
	if _, ok := tlsHint(errors.New("connection refused"), "tuzy.dev"); ok {
		t.Fatal("plain network error classified as TLS")
	}
	cert := func(org string) *tls.ConnectionState {
		return &tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{{Issuer: pkix.Name{Organization: []string{org}}}}}
	}
	if res := certResult(Config{}, cert("Corp Inspection CA")); res.Status != Warn || !strings.Contains(res.Detail, "via proxy") {
		t.Fatalf("%+v", res)
	}
	if res := certResult(Config{}, cert("Let's Encrypt")); res.Status != OK {
		t.Fatalf("%+v", res)
	}
}
