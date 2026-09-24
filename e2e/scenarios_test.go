//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/api"
)

// uniqueName returns a fresh, valid tunnel name so scenarios never collide (and never trip the
// dev edge's RL_CONNECT 10/min-per-name rate limit from a previous run: TestMain wipes D1 state,
// but within one run every scenario still gets its own name to be safe).
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, randomHex(5))
}

// apiClient returns a typed client for the edge's own token (see internal/api), used wherever a
// scenario is testing the REST API directly rather than the CLI.
func (hh *harness) apiClient(t *testing.T, token string) *api.Client {
	t.Helper()
	u, err := url.Parse(fmt.Sprintf("http://localhost:%d", hh.edgePort))
	if err != nil {
		t.Fatal(err)
	}
	return api.New(u, token, "tuzy-e2e/test")
}

// --- 1: GET/POST 50 MB with sha256 parity -------------------------------------------------------

func TestBigBodyShaParity(t *testing.T) {
	u := h.seedUser(t, "bigbody", seedOpts{})
	name := uniqueName("big")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	t.Run("GET", func(t *testing.T) {
		req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/big?mb=50", nil)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("GET /big: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		want := resp.Header.Get("X-Body-Sha256")
		if want == "" {
			t.Fatal("missing X-Body-Sha256 header")
		}
		sum := sha256.New()
		n, err := io.Copy(sum, resp.Body)
		if err != nil {
			t.Fatalf("reading body: %v", err)
		}
		if n != 50*1<<20 {
			t.Fatalf("got %d bytes, want %d", n, 50*1<<20)
		}
		if got := hex.EncodeToString(sum.Sum(nil)); got != want {
			t.Fatalf("sha256 mismatch: got %s want %s", got, want)
		}
	})

	t.Run("POST", func(t *testing.T) {
		const mb = 50
		body := make([]byte, mb*1<<20)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(body)
		req := h.newVisitorRequest(t, http.MethodPost, h.tunnelHost(name), "/echo", bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("POST /echo: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d: %s", resp.StatusCode, b)
		}
		if got := resp.Header.Get("X-Body-Sha256"); got != hex.EncodeToString(want[:]) {
			t.Fatalf("sha256 mismatch: got %s want %x", got, want)
		}
		if got := resp.Header.Get("X-Body-Bytes"); got != strconv.Itoa(len(body)) {
			t.Fatalf("bytes mismatch: got %s want %d", got, len(body))
		}
	})
}

// --- 2: SSE streaming ----------------------------------------------------------------------------

func TestSSEStreaming(t *testing.T) {
	u := h.seedUser(t, "sse", seedOpts{})
	name := uniqueName("sse")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/sse", nil).WithContext(ctx)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	seen := 0
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(line, "data: "))
		if err != nil {
			t.Fatalf("bad event %q: %v", line, err)
		}
		if n != seen {
			t.Fatalf("event out of order: got %d want %d", n, seen)
		}
		seen++
		if seen >= 5 {
			break
		}
	}
	if seen < 5 {
		t.Fatalf("only saw %d events before the stream ended: %v", seen, sc.Err())
	}
}

// --- 3: WS echo ------------------------------------------------------------------------------

// TestWebSocketEcho currently reproduces a product bug rather than passing: see the PRODUCT BUG
// note below. It is kept red on purpose (not skipped) so a fix in edge/src or internal/tunnel is
// verified by this exact test, instead of adding a workaround here.
//
// PRODUCT BUG (found while writing this suite, not fixed here per file ownership):
//
//	Every visitor WebSocket through a tunnel fails instantly with edge-synthesized 502 "Bad
//	gateway" ("x-tuzy-edge: 1", body from pages/status-pages.ts "bad_gateway"), even though the
//	CLI agent's own local upgrade to the sample app succeeds (its access log shows "GET /ws 101"
//	completing in ~2ms — an order of magnitude too fast for a WS session that should stay open
//	until either side closes it).
//
//	Root cause (traced, not confirmed against production): edge/src/protocol/http-stream.ts
//	`start()` registers `signal.addEventListener("abort", () => this.reset("cancelled"))` on the
//	Durable Object's incoming Request signal (`compatibility_flags: ["enable_request_signal"]` in
//	edge/wrangler.jsonc). For a `kind:"ws"` stream this abort appears to fire almost immediately
//	under `wrangler dev` — before the agent's RES_HEAD(101) frame is processed — so `reset()` at
//	edge/src/protocol/http-stream.ts:293 answers the visitor with statusPage("bad_gateway") while
//	`headDone` is still false. The agent's later RES_HEAD then hits the
//	`if (this.headDone || this.done) return this.protocolError()` guard (http-stream.ts:146) and
//	is silently dropped (reset()'s own `if (this.done) return` no-ops), and the RESET the edge
//	sends back to the agent (http-stream.ts:295, notifyAgent=true) makes
//	internal/tunnel/ws_stream.go's `run()` tear the session down within a couple of ms
//	(`case protocol.Reset: w.localClose(1001, "tunnel stream reset")`).
//	Every other stream kind (GET/POST/SSE/file/https) is unaffected — the failure is specific to
//	kind:"ws" (i.e. to responses that resolve `this.head` with a 101/`webSocket` Response instead
//	of a normal body stream) — consistent with the abort being tied to the WebSocketPair handoff
//	rather than to `enable_request_signal` in general.
//
// Repro outside this suite: build ./cmd/tuzy, run it against a local `wrangler dev` edge and any
// local WS echo server, then WebSocket-dial the tunnel host; the edge answers 502 in single-digit
// milliseconds while the CLI's own access log reports a successful local 101.
func TestWebSocketEcho(t *testing.T) {
	u := h.seedUser(t, "ws", seedOpts{})
	name := uniqueName("wsecho")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", h.edgePort)
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{Host: h.tunnelHost(name)})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("hello-%d", i)
		if err := c.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, got, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != msg {
			t.Fatalf("echo mismatch: got %q want %q", got, msg)
		}
	}
	_ = c.Close(websocket.StatusNormalClosure, "done")
}

// --- 4: early 413 ----------------------------------------------------------------------------

func TestEarly413(t *testing.T) {
	u := h.seedUser(t, "early413", seedOpts{})
	name := uniqueName("early413")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	body := make([]byte, 5*1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req := h.newVisitorRequest(t, http.MethodPost, h.tunnelHost(name), "/early413", bytes.NewReader(body)).WithContext(ctx)
	req.ContentLength = int64(len(body))
	start := time.Now()
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("POST /early413: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("413 took %s (app never read the body, should be fast)", elapsed)
	}
}

// --- 5: slow-reader isolation ------------------------------------------------------------------

func TestSlowReaderIsolation(t *testing.T) {
	u := h.seedUser(t, "slow", seedOpts{})
	name := uniqueName("slow")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	slowDone := make(chan error, 1)
	go func() {
		req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/slow?duration_ms=4000", nil)
		resp, err := h.client.Do(req)
		if err != nil {
			slowDone <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, err = io.Copy(io.Discard, resp.Body)
		slowDone <- err
	}()
	time.Sleep(300 * time.Millisecond) // let the slow request actually start streaming

	start := time.Now()
	req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/echo", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("fast request failed while a slow one was in flight: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("fast request took %s while a slow visitor was active; not isolated", elapsed)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow request errored: %v", err)
	}
}

// --- 6: name quota of 10 (via the API) -----------------------------------------------------------

func TestNameQuota(t *testing.T) {
	u := h.seedUser(t, "quota", seedOpts{maxNames: 10})
	c := h.apiClient(t, u.Token)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := c.AddName(ctx, uniqueName(fmt.Sprintf("quota%d", i))); err != nil {
			t.Fatalf("AddName #%d: %v", i, err)
		}
	}
	_, err := c.AddName(ctx, uniqueName("quota-overflow"))
	if err == nil {
		t.Fatal("11th name should have been rejected")
	}
	if !api.IsCode(err, "quota_exceeded") {
		t.Fatalf("err = %v, want quota_exceeded", err)
	}
	var apiErr *api.Error
	if ok := asAPIError(err, &apiErr); !ok || apiErr.Status != http.StatusForbidden {
		t.Fatalf("err = %#v, want status 403", err)
	}
}

func asAPIError(err error, target **api.Error) bool {
	e, ok := err.(*api.Error)
	if ok {
		*target = e
	}
	return ok
}

// --- 7: rename live + 410 on the old name -------------------------------------------------------

func TestRenameLive(t *testing.T) {
	u := h.seedUser(t, "rename", seedOpts{})
	oldName := uniqueName("rn-old")
	newName := uniqueName("rn-new")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", oldName, "--no-inspect", "--quiet")
	h.waitOnline(t, oldName, onlineTimeout)

	c := h.apiClient(t, u.Token)
	if _, err := c.RenameName(context.Background(), oldName, newName); err != nil {
		t.Fatalf("RenameName: %v", err)
	}
	// The running agent must follow: the NEW name should come online without restarting it.
	h.waitOnline(t, newName, onlineTimeout)

	// A fresh connect attempt against the OLD name must be told it was renamed (410).
	status, body := h.rawConnectAttempt(t, u.Token, oldName)
	if status != http.StatusGone {
		t.Fatalf("connect to renamed name: status = %d, body = %s", status, body)
	}
	if !strings.Contains(body, newName) {
		t.Fatalf("410 body should mention the new name %q: %s", newName, body)
	}
}

// rawConnectAttempt hits GET /api/v1/connect directly (an Upgrade: websocket request) and returns
// before any 101 handshake completes for a rejection (400/401/403/410/429): the edge's own
// pre-upgrade checks (auth, name validity, connectAuthz) always answer as a normal HTTP response.
func (hh *harness) rawConnectAttempt(t *testing.T, token, name string) (int, string) {
	t.Helper()
	instance := "e2e" + randomHex(16)
	path := fmt.Sprintf("/api/v1/connect?name=%s&instance=%s", url.QueryEscape(name), instance)
	req := hh.newVisitorRequest(t, http.MethodGet, hh.apexHost(), path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	resp, err := hh.client.Do(req)
	if err != nil {
		t.Fatalf("connect attempt: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// --- 8: --force takeover --------------------------------------------------------------------

func TestForceTakeover(t *testing.T) {
	u := h.seedUser(t, "force", seedOpts{})
	name := uniqueName("force")
	first := h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--force", "--no-inspect", "--quiet")

	if err := first.wait(15 * time.Second); err == nil {
		t.Fatal("the first agent should exit (GOAWAY replaced) once --force takes over the name")
	}
	// The new holder should still be serving (waitOnline tolerates the brief handover window).
	h.waitOnline(t, name, onlineTimeout)
}

// --- 9: token revoke ends the tunnel -----------------------------------------------------------

func TestTokenRevoke(t *testing.T) {
	u := h.seedUser(t, "revoke", seedOpts{})
	name := uniqueName("revoke")
	p := h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	c := h.apiClient(t, u.Token)
	if err := c.RevokeToken(context.Background(), "current"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if err := p.wait(15 * time.Second); err == nil {
		t.Fatal("the agent should exit once its token is revoked")
	}
	ok := waitFor(t, 10*time.Second, 300*time.Millisecond, func() bool {
		req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/health", nil)
		resp, err := h.client.Do(req)
		if err != nil {
			return true
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.Header.Get("X-Tuzy-Edge") != ""
	})
	if !ok {
		t.Fatal("tunnel should stop serving the app after its token was revoked")
	}
}

// --- 10: internal paths reach the local app --------------------------------------------------

func TestInternalPathsReachLocalApp(t *testing.T) {
	u := h.seedUser(t, "internalpath", seedOpts{})
	name := uniqueName("internal")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/api/v1/health", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/health on tunnel host: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("X-Tuzy-Edge") != "" {
		t.Fatal("the edge answered instead of the local app")
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding app response: %v", err)
	}
	if body["path"] != "/api/v1/health" {
		t.Fatalf("app did not see the internal-looking path: %v", body)
	}
	if _, isEdgeShape := body["min_proto"]; isEdgeShape {
		t.Fatal("response looks like the edge's own /api/v1/health, not the app's")
	}
}

// --- 11: interstitial ------------------------------------------------------------------------

func TestInterstitial(t *testing.T) {
	u := h.seedUser(t, "interstitial", seedOpts{})
	name := uniqueName("intstl")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)
	host := h.tunnelHost(name)

	t.Run("document navigation is shown the interstitial", func(t *testing.T) {
		req := h.newVisitorRequest(t, http.MethodGet, host, "/echo", nil)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "about to visit a tunnel") {
			t.Fatalf("status=%d body=%s", resp.StatusCode, b)
		}
		if resp.Header.Get("X-Tuzy-Edge") != "1" {
			t.Fatal("interstitial should be edge-served")
		}
	})

	t.Run("webhook POST passes straight through", func(t *testing.T) {
		req := h.newVisitorRequest(t, http.MethodPost, host, "/echo", strings.NewReader(`{"hook":true}`))
		// No Sec-Fetch-* headers and no text/html Accept: exactly what a webhook sender sends.
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("webhook should pass straight through, got %d: %s", resp.StatusCode, b)
		}
	})

	t.Run("tuzy-skip-warning skips it", func(t *testing.T) {
		req := h.newVisitorRequest(t, http.MethodGet, host, "/echo", nil)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("tuzy-skip-warning", "1")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || strings.Contains(string(b), "about to visit a tunnel") {
			t.Fatalf("tuzy-skip-warning should reach the app: status=%d body=%s", resp.StatusCode, b)
		}
	})

	t.Run("continue click mints a cookie that then passes navigations through", func(t *testing.T) {
		origin := "http://" + host
		req := h.newVisitorRequest(t, http.MethodGet, host, "/__tuzy/continue?to=/echo", nil)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Referer", origin+"/")
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusFound {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("continue: status=%d body=%s", resp.StatusCode, b)
		}
		var cookie string
		for _, c := range resp.Cookies() {
			if c.Name == "tuzy_ok" {
				cookie = c.String()
			}
		}
		if cookie == "" {
			t.Fatal("continue click should mint a tuzy_ok cookie")
		}

		req2 := h.newVisitorRequest(t, http.MethodGet, host, "/echo", nil)
		req2.Header.Set("Sec-Fetch-Dest", "document")
		req2.Header.Set("Sec-Fetch-Mode", "navigate")
		req2.Header.Set("Sec-Fetch-Site", "none")
		req2.Header.Set("Cookie", cookie)
		resp2, err := h.client.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp2.Body.Close() }()
		b2, _ := io.ReadAll(resp2.Body)
		if resp2.StatusCode != http.StatusOK || strings.Contains(string(b2), "about to visit a tunnel") {
			t.Fatalf("cookie should let the navigation through: status=%d body=%s", resp2.StatusCode, b2)
		}
	})
}

// --- 12: suspend -> 451 ----------------------------------------------------------------------

func TestSuspendedNameAnswers451(t *testing.T) {
	owner := h.seedUser(t, "suspendowner", seedOpts{})
	admin := h.seedUser(t, "suspendadmin", seedOpts{admin: true})
	name := uniqueName("suspend")
	h.startCLI(t, "", owner.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	c := h.apiClient(t, admin.Token)
	if _, err := c.AdminPost(context.Background(), "/names/"+name+"/suspend", nil); err != nil {
		t.Fatalf("admin suspend: %v", err)
	}

	ok := waitFor(t, 10*time.Second, 300*time.Millisecond, func() bool {
		req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/health", nil)
		resp, err := h.client.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusUnavailableForLegalReasons
	})
	if !ok {
		t.Fatal("visitors should get 451 once the name is suspended")
	}
}

// --- 13: file:// and https:// upstreams --------------------------------------------------------

func TestFileUpstream(t *testing.T) {
	u := h.seedUser(t, "fileupstream", seedOpts{})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "known.txt"), []byte("hello from disk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := uniqueName("filetgt")
	h.startCLI(t, "", u.Token, "http", "file://"+dir, "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/known.txt", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "hello from disk\n" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, b)
	}
}

func TestHTTPSUpstream(t *testing.T) {
	u := h.seedUser(t, "httpsupstream", seedOpts{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "tls upstream %s", r.URL.Path)
	}))
	defer upstream.Close()

	name := uniqueName("httpstgt")
	h.startCLI(t, "", u.Token, "http", upstream.URL, "--upstream-insecure", "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/ping", nil)
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(b) != "tls upstream /ping" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, b)
	}
}

// --- 14: diagnose --json ----------------------------------------------------------------------

func TestDiagnoseJSON(t *testing.T) {
	u := h.seedUser(t, "diagnose", seedOpts{})
	out, err := h.runCLIOnce(t, 20*time.Second, u.Token, "diagnose", "--json", "--server", fmt.Sprintf("http://localhost:%d", h.edgePort))
	if err != nil {
		t.Fatalf("tuzy diagnose --json failed: %v\n%s", err, out)
	}
	var report struct {
		OK     bool `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !report.OK {
		t.Fatalf("diagnose reported failure:\n%s", out)
	}
	byName := map[string]string{}
	for _, c := range report.Checks {
		byName[c.Name] = c.Status
	}
	if byName["tls"] != "skip" {
		t.Fatalf("tls check on an http:// server should be skipped, got %q", byName["tls"])
	}
	for _, name := range []string{"api", "websocket", "token"} {
		if byName[name] != "ok" {
			t.Fatalf("check %q = %q, want ok (full report: %s)", name, byName[name], out)
		}
	}
}

// --- 15: start --all + shared inspector --------------------------------------------------------

func TestStartAllSharedInspector(t *testing.T) {
	u := h.seedUser(t, "startall", seedOpts{})
	dir := t.TempDir()
	web := uniqueName("web")
	api2 := uniqueName("api")
	toml := fmt.Sprintf("[tunnels.%s]\naddr = \"127.0.0.1:%d\"\n\n[tunnels.%s]\naddr = \"127.0.0.1:%d\"\n", web, h.appPort, api2, h.appPort)
	if err := os.WriteFile(filepath.Join(dir, "tuzy.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	inspectPort := 14099
	if err := requireFreePort(inspectPort, "(fixed e2e inspector port)"); err != nil {
		t.Fatalf("inspector port busy: %v", err)
	}
	h.startCLI(t, dir, u.Token, "start", "--all", "--inspect-addr", fmt.Sprintf("127.0.0.1:%d", inspectPort), "--quiet")
	h.waitOnline(t, web, onlineTimeout)
	h.waitOnline(t, api2, onlineTimeout)

	for _, name := range []string{web, api2} {
		req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/echo?tag="+name, nil)
		resp, err := h.client.Do(req)
		if err != nil {
			t.Fatalf("request through %s: %v", name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	var requests []struct {
		Tunnel string `json:"tunnel"`
	}
	ok := waitFor(t, 5*time.Second, 200*time.Millisecond, func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/requests", inspectPort))
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		requests = nil
		if err := json.NewDecoder(resp.Body).Decode(&requests); err != nil {
			return false
		}
		return len(requests) >= 2
	})
	if !ok {
		t.Fatalf("inspector never listed both tunnels' requests: %+v", requests)
	}
	seen := map[string]bool{}
	for _, r := range requests {
		seen[r.Tunnel] = true
	}
	if !seen[web] || !seen[api2] {
		t.Fatalf("inspector should list requests for both %q and %q: %+v", web, api2, requests)
	}
}

// --- 16: account delete (skipped: infeasible without reading the emailed code) -------------------

func TestAccountDelete(t *testing.T) {
	t.Skip("infeasible locally: the log EMAIL_DRIVER records the deletion code only in the Worker's " +
		"in-process memory (edge/src/lib/email.ts sentEmails), which this out-of-process Go test " +
		"harness cannot read; the OTP/code flow itself is covered by the edge's own vitest suite")
}

// --- 17: long-stream budget cut (slow; opt-in) --------------------------------------------------

func TestLongStreamBudgetCut(t *testing.T) {
	if os.Getenv("TUZY_E2E_SLOW") == "" {
		t.Skip("slow by construction: the dev config's LONG_STREAM_AFTER_SECONDS=300 means a stream " +
			"only starts counting against the monthly budget after 5 minutes, so proving a cut takes " +
			"5+ minutes; set TUZY_E2E_SLOW=1 to run it")
	}
	u := h.seedUser(t, "budget", seedOpts{})
	c := h.apiClient(t, u.Token)
	me, err := c.Me(context.Background())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.Usage == nil || me.Usage.LongStreamBudgetSeconds <= 0 {
		t.Fatalf("no long-stream budget reported: %+v", me.Usage)
	}
	budget := me.Usage.LongStreamBudgetSeconds
	month := time.Now().UTC().Format("2006-01")
	h.d1Exec(t, fmt.Sprintf(
		"INSERT INTO usage_monthly (user_id,month,long_stream_seconds) VALUES (%s,%s,%d) ON CONFLICT(user_id,month) DO UPDATE SET long_stream_seconds=%d;",
		sqlQuote(u.ID), sqlQuote(month), budget, budget))

	name := uniqueName("budget")
	h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect", "--quiet")
	h.waitOnline(t, name, onlineTimeout)

	ctx, cancel := context.WithTimeout(context.Background(), 340*time.Second)
	defer cancel()
	req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/sse", nil).WithContext(ctx)
	start := time.Now()
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body) // the sample app streams for up to 60s on its own; expect an earlier edge cut
	elapsed := time.Since(start)
	if elapsed < 250*time.Second || elapsed > 330*time.Second {
		t.Fatalf("expected the edge to cut the stream around LONG_STREAM_AFTER_SECONDS=300s once at budget, got %s", elapsed)
	}
}

// --- edge redeploy: agents reconnect after the Durable Object restarts -------------------------

// TestEdgeReloadReconnect hot-reloads `wrangler dev` (new script → every Durable Object restarts,
// like a production deploy) while a tunnel is live, and requires the SAME agent process to come
// back and keep serving. Regression: after a deploy an agent got stuck in a "no HELLO" loop until
// it was restarted.
func TestEdgeReloadReconnect(t *testing.T) {
	u := h.seedUser(t, "reload", seedOpts{})
	name := uniqueName("reload")
	agent := h.startCLI(t, "", u.Token, "http", fmt.Sprintf("127.0.0.1:%d", h.appPort), "--name", name, "--no-inspect")
	h.waitOnline(t, name, onlineTimeout)

	// Each reload needs a different bundle (wrangler skips identical rebuilds): append a comment.
	touched := filepath.Join(h.edgeDir, "src", "index.ts")
	orig, err := os.ReadFile(touched)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(touched, orig, 0o644) })
	for i := range 4 {
		before := strings.Count(agent.String(), "● online")
		if err := os.WriteFile(touched, fmt.Appendf(bytes.Clone(orig), "\n// e2e reload %d\n", i+1), 0o644); err != nil {
			t.Fatal(err)
		}
		// The reload drops the agent socket; wait for a fresh "online", then for real traffic.
		if !waitFor(t, 60*time.Second, 200*time.Millisecond, func() bool { return strings.Count(agent.String(), "● online") > before }) {
			t.Fatalf("reload %d: agent did not reconnect within 60s\n%s", i+1, agent.String())
		}
		h.waitOnline(t, name, onlineTimeout)
		// A request right after the reconnect (the stuck case followed a request).
		req := h.newVisitorRequest(t, http.MethodGet, h.tunnelHost(name), "/health", nil)
		resp, err := h.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("reload %d: request after reconnect: %v %v\n%s", i+1, err, resp, agent.String())
		}
		_ = resp.Body.Close()
		if strings.Contains(agent.String(), "no HELLO") {
			t.Fatalf("reload %d: agent hit a no-HELLO loop\n%s", i+1, agent.String())
		}
	}
	t.Logf("agent output:\n%s", agent.String())
}
