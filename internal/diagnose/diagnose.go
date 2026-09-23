// Package diagnose runs ordered connectivity checks against the tuzy edge (`tuzy diagnose`):
// proxy settings, DNS, TLS (with an interception hint), API health and clock skew, a WebSocket
// round trip through any proxy, the login token, and the local inspector port.
package diagnose

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Status of one check.
type Status string

// Check outcomes.
const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Result is one check's outcome.
type Result struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// Config drives a run. Zero values pick production defaults.
type Config struct {
	Server      *url.URL
	Token       string // empty: not logged in
	TokenErr    error  // why no token is available (reported, not fatal)
	UserAgent   string
	Proto       int    // protocol version this CLI speaks
	InspectAddr string // default 127.0.0.1:4040
	Timeout     time.Duration
	RootCAs     *x509.CertPool // tests: trust a test server
	Now         func() time.Time
}

// PublicCAHints are issuer organisations the edge certificate is expected to come from; anything
// else suggests a TLS-intercepting proxy (a hint only: the check still passes if the chain verifies).
var PublicCAHints = []string{"Google Trust Services", "Let's Encrypt", "Cloudflare", "DigiCert", "Sectigo", "GlobalSign", "SSL.com"}

// Run executes every check in order.
func Run(ctx context.Context, cfg Config) []Result {
	if cfg.Timeout == 0 {
		cfg.Timeout = 8 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.InspectAddr == "" {
		cfg.InspectAddr = "127.0.0.1:4040"
	}
	tlsCfg := &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}
	client := &http.Client{
		Timeout:       cfg.Timeout,
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: tlsCfg},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	proxy := proxyFor(cfg.Server)
	out := []Result{checkProxy(proxy)}
	dns := checkDNS(ctx, cfg, proxy)
	out = append(out, dns)
	health := checkHealth(ctx, cfg, client)
	if proxy != nil && cfg.Server.Scheme == "https" {
		out = append(out, health.viaProxyTLS) // the direct dial would bypass the proxy
	} else {
		out = append(out, checkTLS(ctx, cfg, tlsCfg))
	}
	out = append(out, health.api, health.clock)
	out = append(out, checkWebSocket(ctx, cfg, client))
	out = append(out, checkToken(ctx, cfg, client))
	out = append(out, checkInspectorPort(cfg.InspectAddr))
	return out
}

// Failed reports whether any check failed.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Status == Fail {
			return true
		}
	}
	return false
}

func proxyFor(server *url.URL) *url.URL {
	req := &http.Request{URL: server}
	p, _ := http.ProxyFromEnvironment(req)
	return p
}

func checkProxy(p *url.URL) Result {
	if p == nil {
		return Result{Name: "proxy", Status: OK, Detail: "no proxy configured (direct connection)"}
	}
	shown := *p
	shown.User = nil // never print proxy credentials
	return Result{Name: "proxy", Status: OK, Detail: "using " + shown.String() + " (from HTTPS_PROXY/HTTP_PROXY)"}
}

func checkDNS(ctx context.Context, cfg Config, proxy *url.URL) Result {
	host := cfg.Server.Hostname()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		if proxy != nil {
			return Result{Name: "dns", Status: Warn, Detail: fmt.Sprintf("%s does not resolve locally (%v)", host, err), Hint: "fine if your proxy resolves names for you"}
		}
		return Result{Name: "dns", Status: Fail, Detail: fmt.Sprintf("cannot resolve %s: %v", host, err), Hint: "check your internet connection and DNS settings"}
	}
	sort.Strings(addrs)
	return Result{Name: "dns", Status: OK, Detail: fmt.Sprintf("%s → %s", host, strings.Join(addrs, ", "))}
}

func checkTLS(ctx context.Context, cfg Config, tlsCfg *tls.Config) Result {
	if cfg.Server.Scheme != "https" {
		return Result{Name: "tls", Status: Skip, Detail: "server is not https"}
	}
	host := cfg.Server.Hostname()
	port := cfg.Server.Port()
	if port == "" {
		port = "443"
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: cfg.Timeout}, Config: tlsCfg.Clone()}
	d.Config.ServerName = host
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	start := time.Now()
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		if r, ok := tlsHint(err, host); ok {
			return r
		}
		return Result{Name: "tls", Status: Fail, Detail: "handshake failed: " + err.Error(), Hint: "a firewall may block port " + port}
	}
	defer func() { _ = conn.Close() }()
	state := conn.(*tls.Conn).ConnectionState()
	leaf := state.PeerCertificates[0]
	issuer := strings.Join(leaf.Issuer.Organization, ", ")
	if issuer == "" {
		issuer = leaf.Issuer.CommonName
	}
	detail := fmt.Sprintf("%s, issuer %s, expires %s (%s)", tls.VersionName(state.Version), issuer, leaf.NotAfter.Format("2006-01-02"), time.Since(start).Round(time.Millisecond))
	if cfg.RootCAs == nil && !knownIssuer(issuer) {
		return Result{Name: "tls", Status: Warn, Detail: detail, Hint: "unexpected certificate issuer: traffic may be inspected by a proxy or antivirus"}
	}
	return Result{Name: "tls", Status: OK, Detail: detail}
}

func knownIssuer(issuer string) bool {
	for _, h := range PublicCAHints {
		if strings.Contains(issuer, h) {
			return true
		}
	}
	return false
}

type healthResults struct{ api, clock, viaProxyTLS Result }

// tlsHint classifies certificate errors (e.g. from a TLS-inspecting proxy with an unknown root).
func tlsHint(err error, host string) (Result, bool) {
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostErr x509.HostnameError
	switch {
	case errors.As(err, &unknown), errors.As(err, &hostErr):
		return Result{Name: "tls", Status: Fail, Detail: "untrusted certificate: " + err.Error(), Hint: "a TLS-intercepting proxy or antivirus is likely; ask IT to exempt " + host}, true
	case errors.As(err, &invalid):
		return Result{Name: "tls", Status: Fail, Detail: err.Error(), Hint: "check your system clock and any TLS-inspecting software"}, true
	}
	return Result{}, false
}

// certResult reports the certificate a (possibly proxied) HTTPS response was served with.
func certResult(cfg Config, state *tls.ConnectionState) Result {
	if state == nil || len(state.PeerCertificates) == 0 {
		return Result{Name: "tls", Status: Skip, Detail: "no TLS information"}
	}
	leaf := state.PeerCertificates[0]
	issuer := strings.Join(leaf.Issuer.Organization, ", ")
	if issuer == "" {
		issuer = leaf.Issuer.CommonName
	}
	detail := fmt.Sprintf("via proxy: %s, issuer %s, expires %s", tls.VersionName(state.Version), issuer, leaf.NotAfter.Format("2006-01-02"))
	if cfg.RootCAs == nil && !knownIssuer(issuer) {
		return Result{Name: "tls", Status: Warn, Detail: detail, Hint: "unexpected certificate issuer: the proxy inspects TLS; WebSockets may break"}
	}
	return Result{Name: "tls", Status: OK, Detail: detail}
}

func checkHealth(ctx context.Context, cfg Config, client *http.Client) healthResults {
	u := *cfg.Server
	u.Path = "/api/v1/health"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", cfg.UserAgent)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		tlsRes, isTLS := tlsHint(err, cfg.Server.Hostname())
		if !isTLS {
			tlsRes = Result{Name: "tls", Status: Skip, Detail: "no connection"}
		}
		return healthResults{
			api:         Result{Name: "api", Status: Fail, Detail: err.Error(), Hint: "the edge is unreachable over HTTPS (proxy, firewall or no internet)"},
			clock:       Result{Name: "clock", Status: Skip, Detail: "needs the API"},
			viaProxyTLS: tlsRes,
		}
	}
	defer func() { _ = resp.Body.Close() }()
	rtt := time.Since(start)
	var h struct {
		OK       bool   `json:"ok"`
		Version  string `json:"version"`
		MinProto int    `json:"min_proto"`
		MaxProto int    `json:"max_proto"`
	}
	decErr := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&h)
	var api Result
	switch {
	case resp.StatusCode != http.StatusOK || decErr != nil || !h.OK:
		api = Result{Name: "api", Status: Fail, Detail: fmt.Sprintf("unexpected answer: %s", resp.Status), Hint: "a proxy or captive portal may be answering instead of tuzy"}
	case cfg.Proto != 0 && h.MaxProto != 0 && (cfg.Proto < h.MinProto || cfg.Proto > h.MaxProto):
		api = Result{Name: "api", Status: Fail, Detail: fmt.Sprintf("edge speaks protocol %d–%d, this CLI %d", h.MinProto, h.MaxProto, cfg.Proto), Hint: "run `tuzy update`"}
	default:
		version := h.Version
		if version == "" {
			version = "(unknown)"
		}
		api = Result{Name: "api", Status: OK, Detail: fmt.Sprintf("healthy, edge %s, %s", version, rtt.Round(time.Millisecond))}
	}
	clock := Result{Name: "clock", Status: Skip, Detail: "no Date header"}
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		skew := cfg.Now().Sub(d) - rtt/2
		abs := skew
		if abs < 0 {
			abs = -abs
		}
		clock = Result{Name: "clock", Status: OK, Detail: fmt.Sprintf("skew %s", skew.Round(time.Second))}
		if abs > 5*time.Minute {
			clock.Status, clock.Hint = Fail, "fix your system clock (TLS and token expiry depend on it)"
		} else if abs > time.Minute {
			clock.Status, clock.Hint = Warn, "enable automatic time sync"
		}
	}
	return healthResults{api: api, clock: clock, viaProxyTLS: certResult(cfg, resp.TLS)}
}

func checkWebSocket(ctx context.Context, cfg Config, client *http.Client) Result {
	u := *cfg.Server
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/api/v1/diagnose/ws"
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"User-Agent": {cfg.UserAgent}}})
	if err != nil {
		status := ""
		if resp != nil {
			status = " (" + resp.Status + ")"
		}
		return Result{Name: "websocket", Status: Fail, Detail: "upgrade failed" + status + ": " + err.Error(), Hint: "a proxy or firewall blocks WebSockets; tunnels need them (ask IT to allow wss://" + cfg.Server.Host + ")"}
	}
	defer func() { _ = c.CloseNow() }()
	var samples []time.Duration
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("tuzy-diagnose-%d", i)
		t := time.Now()
		if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			return Result{Name: "websocket", Status: Fail, Detail: "write: " + err.Error()}
		}
		_, got, err := c.Read(ctx)
		if err != nil || string(got) != msg {
			return Result{Name: "websocket", Status: Fail, Detail: fmt.Sprintf("echo broken: %v", err), Hint: "a proxy is interfering with WebSocket frames"}
		}
		samples = append(samples, time.Since(t))
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
	parts := make([]string, len(samples))
	for i, s := range samples {
		parts[i] = s.Round(time.Millisecond).String()
	}
	return Result{Name: "websocket", Status: OK, Detail: "echo RTT " + strings.Join(parts, " / ")}
}

func checkToken(ctx context.Context, cfg Config, client *http.Client) Result {
	if cfg.Token == "" {
		detail := "not logged in"
		if cfg.TokenErr != nil && !strings.Contains(cfg.TokenErr.Error(), "not logged in") {
			detail = cfg.TokenErr.Error()
		}
		return Result{Name: "token", Status: Warn, Detail: detail, Hint: "run `tuzy login` (or set TUZY_TOKEN in CI)"}
	}
	u := *cfg.Server
	u.Path = "/api/v1/me"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("User-Agent", cfg.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Result{Name: "token", Status: Skip, Detail: "API unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()
	var me struct {
		User  struct{ Email string }         `json:"user"`
		Token struct{ Scope, Label string }  `json:"token"`
		Error struct{ Code, Message string } `json:"error"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&me)
	switch resp.StatusCode {
	case http.StatusOK:
		return Result{Name: "token", Status: OK, Detail: fmt.Sprintf("%s · scope %s · %s", me.User.Email, me.Token.Scope, me.Token.Label)}
	case http.StatusUnauthorized:
		return Result{Name: "token", Status: Fail, Detail: me.Error.Message, Hint: "run `tuzy login`"}
	default:
		return Result{Name: "token", Status: Fail, Detail: fmt.Sprintf("%s: %s", resp.Status, me.Error.Message)}
	}
}

func checkInspectorPort(addr string) Result {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return Result{Name: "inspector", Status: Warn, Detail: addr + " is in use", Hint: "the inspector will try the next ports (up to 4049) or use --inspect-addr"}
	}
	_ = ln.Close()
	return Result{Name: "inspector", Status: OK, Detail: addr + " is free"}
}
