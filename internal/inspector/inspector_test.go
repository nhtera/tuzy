package inspector

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/nhtera/tuzy/internal/protocol"
)

func record(s *Store, tunnel, method, path string, reqBody, resBody []byte) uint64 {
	r := s.Begin(tunnel, protocol.ReqHeadMsg{Method: method, Path: path, Kind: "http", Headers: []protocol.Header{{"host", "shop.tuzy.dev"}, {"content-type", "text/plain"}}})
	r.RequestBody(reqBody)
	r.RequestEnd() // REQ_END on the wire, as a real (complete) request always sees
	r.Response(200, []protocol.Header{{"content-type", "text/plain"}})
	r.ResponseBody(resBody)
	r.End("")
	return r.(*streamRec).id
}

func TestEvictionByCountAndBytes(t *testing.T) {
	s := NewStore()
	for i := 0; i < MaxEntries+20; i++ {
		record(s, "a", "GET", "/", nil, nil)
	}
	if n := len(s.List(Filter{})); n != MaxEntries {
		t.Fatalf("entries = %d, want %d", n, MaxEntries)
	}
	s.Clear()
	big := bytes.Repeat([]byte("x"), MaxBodyCapture)
	for i := 0; i < 100; i++ { // 100 × 2 MiB captured
		record(s, "a", "POST", "/", big, big)
	}
	if tb := s.totalBytes(); tb > MaxTotalBytes {
		t.Fatalf("captured %d bytes > budget %d", tb, MaxTotalBytes)
	}
	list := s.List(Filter{})
	oldest, _ := s.Get(list[len(list)-1].ID)
	newest, _ := s.Get(list[0].ID)
	if !oldest.Evicted || len(oldest.ReqBody) != 0 || newest.Evicted || len(newest.ReqBody) != MaxBodyCapture {
		t.Fatalf("expected oldest bodies evicted first (oldest evicted=%v, newest evicted=%v)", oldest.Evicted, newest.Evicted)
	}
}

func TestTruncationFlag(t *testing.T) {
	s := NewStore()
	id := record(s, "a", "POST", "/", bytes.Repeat([]byte("y"), MaxBodyCapture+10), []byte("ok"))
	e, _ := s.Get(id)
	if !e.ReqTrunc || len(e.ReqBody) != MaxBodyCapture || e.ReqSize != MaxBodyCapture+10 || e.ResTrunc {
		t.Fatalf("trunc=%v len=%d size=%d", e.ReqTrunc, len(e.ReqBody), e.ReqSize)
	}
}

func TestFilters(t *testing.T) {
	s := NewStore()
	a := s.Begin("web", protocol.ReqHeadMsg{Method: "GET", Path: "/a", Kind: "http"})
	a.Response(404, nil)
	a.End("")
	b := s.Begin("api", protocol.ReqHeadMsg{Method: "POST", Path: "/b", Kind: "http"})
	b.Response(201, nil)
	b.End("")
	if n := len(s.List(Filter{Tunnel: "api"})); n != 1 {
		t.Fatalf("tunnel filter: %d", n)
	}
	if n := len(s.List(Filter{Status: "4xx"})); n != 1 {
		t.Fatalf("status class filter: %d", n)
	}
	if n := len(s.List(Filter{Method: "post", Status: "201"})); n != 1 {
		t.Fatalf("method+status filter: %d", n)
	}
}

// startServer returns a running inspector with one tunnel pointing at app.
func startServer(t *testing.T, app *httptest.Server) (*Server, *Store) {
	t.Helper()
	s := NewStore()
	target, _ := url.Parse(app.URL)
	s.SetTunnel(TunnelInfo{Name: "shop", PublicURL: "https://shop.tuzy.dev", Target: target, HostHeader: "preserve"})
	srv, err := Listen("127.0.0.1:0", s, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, s
}

func mutate(t *testing.T, srv *Server, method, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, srv.URL()+path, bytes.NewReader(b))
	req.Header.Set("X-Tuzy-Inspector", "1")
	req.Header.Set("Origin", srv.URL())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReplayParityAndEditedReplay(t *testing.T) {
	type got struct {
		method, path, host, hdr string
		body                    []byte
	}
	seen := make(chan got, 4)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen <- got{r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("X-Signature"), b}
		_, _ = w.Write([]byte("pong"))
	}))
	defer app.Close()
	srv, s := startServer(t, app)
	r := s.Begin("shop", protocol.ReqHeadMsg{Method: "POST", Path: "/hook?a=1", Kind: "http",
		Headers: []protocol.Header{{"host", "shop.tuzy.dev"}, {"x-signature", "sig123"}, {"content-type", "application/json"}}})
	body := []byte(`{"event":"paid","amount":42}`)
	r.RequestBody(body)
	r.RequestEnd()
	r.Response(200, nil)
	r.End("")
	id := r.(*streamRec).id

	res := mutate(t, srv, "POST", fmt.Sprintf("/api/requests/%d/replay", id), map[string]any{})
	if res.StatusCode != 200 {
		t.Fatalf("replay status %d", res.StatusCode)
	}
	g := <-seen
	if g.method != "POST" || g.path != "/hook?a=1" || g.host != "shop.tuzy.dev" || g.hdr != "sig123" || !bytes.Equal(g.body, body) {
		t.Fatalf("replay not identical: %+v %q", g, g.body)
	}
	var out struct{ ID uint64 }
	_ = json.NewDecoder(res.Body).Decode(&out)
	e, _ := s.Get(out.ID)
	if e.ReplayOf != id || e.Status != 200 || string(e.ResBody) != "pong" {
		t.Fatalf("replay entry %+v", e)
	}

	res = mutate(t, srv, "POST", "/api/replay", map[string]any{"tunnel": "shop", "method": "PUT", "path": "/edited", "headers": [][2]string{{"X-Signature", "new"}}, "body_b64": "ZWRpdGVk"})
	if res.StatusCode != 200 {
		t.Fatalf("edited replay status %d", res.StatusCode)
	}
	g = <-seen
	if g.method != "PUT" || g.path != "/edited" || g.hdr != "new" || string(g.body) != "edited" {
		t.Fatalf("edited replay got %+v", g)
	}
}

func TestHostOriginAndHeaderGuards(t *testing.T) {
	app := httptest.NewServer(http.NotFoundHandler())
	defer app.Close()
	srv, _ := startServer(t, app)

	req, _ := http.NewRequest("GET", srv.URL()+"/api/requests", nil)
	req.Host = "evil.com"
	res, _ := http.DefaultClient.Do(req)
	if res.StatusCode != 403 {
		t.Fatalf("rebinding Host: %d", res.StatusCode)
	}
	if !strings.Contains(res.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("CSP missing on error responses")
	}
	// Cross-origin mutation (a malicious page): wrong Origin, and missing custom header.
	for _, h := range []map[string]string{{"Origin": "https://evil.com", "X-Tuzy-Inspector": "1"}, {"Origin": srv.URL()}} {
		req, _ = http.NewRequest("DELETE", srv.URL()+"/api/requests", nil)
		for k, v := range h {
			req.Header.Set(k, v)
		}
		res, _ = http.DefaultClient.Do(req)
		if res.StatusCode != 403 {
			t.Fatalf("mutation with %v: %d", h, res.StatusCode)
		}
	}
	res, _ = http.Get(srv.URL() + "/")
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !bytes.Contains(b, []byte("tuzy inspector")) || res.Header.Get("Content-Security-Policy") == "" {
		t.Fatalf("UI: %d", res.StatusCode)
	}
}

func TestListenRefusesNonLoopback(t *testing.T) {
	if _, err := Listen("0.0.0.0:0", NewStore(), http.DefaultTransport); err == nil {
		t.Fatal("expected loopback-only refusal")
	}
}

// hostileFixture is one testdata/hostile.json entry (script tags, quote-breaking shell metachars,
// NUL bytes, RTL overrides…). Field names are matched case-insensitively by encoding/json.
type hostileFixture struct {
	Path   string
	Header [2]string
	Body   string
}

func readHostileFixtures(t *testing.T) []hostileFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/hostile.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []hostileFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no hostile fixtures")
	}
	return fixtures
}

// TestHostileDataIsEscapedJSON is M7: every hostile fixture round-trips through the JSON API
// (list and detail) with no raw markup, whatever the path/header/body throws at the encoder.
func TestHostileDataIsEscapedJSON(t *testing.T) {
	fixtures := readHostileFixtures(t)
	app := httptest.NewServer(http.NotFoundHandler())
	defer app.Close()
	srv, s := startServer(t, app)
	for _, h := range fixtures {
		r := s.Begin("shop", protocol.ReqHeadMsg{Method: "GET", Path: h.Path, Kind: "http", Headers: []protocol.Header{h.Header}})
		r.RequestBody([]byte(h.Body))
		r.End("")
	}
	assertNoRawMarkup := func(b []byte) {
		t.Helper()
		for _, bad := range [][]byte{[]byte("<script>"), []byte("<img"), []byte("<svg")} {
			if bytes.Contains(b, bad) {
				t.Fatalf("raw markup %q in JSON: %s", bad, b)
			}
		}
	}
	res, err := http.Get(srv.URL() + "/api/requests")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	assertNoRawMarkup(b)
	if res.Header.Get("Content-Type") != "application/json" {
		t.Fatal("API must be served as JSON")
	}
	var list []Summary
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	for _, sum := range list {
		dres, err := http.Get(fmt.Sprintf("%s/api/requests/%d", srv.URL(), sum.ID))
		if err != nil {
			t.Fatal(err)
		}
		db, _ := io.ReadAll(dres.Body)
		assertNoRawMarkup(db)
	}
}

func TestUINeverUsesInnerHTML(t *testing.T) {
	js, _ := uiFS.ReadFile("ui/app.js")
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		// the header comment mentions innerHTML once to document the rule
		if strings.Count(string(js), bad) > strings.Count("never innerHTML", bad) {
			t.Fatalf("app.js uses %s", bad)
		}
	}
	html, _ := uiFS.ReadFile("ui/index.html")
	if bytes.Contains(html, []byte("<script>")) || bytes.Contains(html, []byte("onclick=")) {
		t.Fatal("index.html must not contain inline script")
	}
}

// TestCurlQuotingRoundTrip is M7: every hostile fixture's path/header/body survives POSIX single
// quoting unexploited — no expansion, no command substitution — for both a text and a binary body.
// The command-substitution marker is a fresh t.TempDir() path each run (not the fixture's literal
// /tmp/pwned) so the test can never false-fail because that file already exists from something
// unrelated.
func TestCurlQuotingRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell quoting")
	}
	fixtures := readHostileFixtures(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	for _, h := range fixtures {
		// argv/shell cannot carry a NUL byte at all (independent of quoting correctness): skip.
		if strings.ContainsRune(h.Path, 0) || strings.ContainsRune(h.Header[1], 0) {
			continue
		}
		body := strings.ReplaceAll(h.Body, "/tmp/pwned", marker)
		for _, b := range [][]byte{[]byte(body), {0, 1, 2, 0xff, '\''}} {
			e := &Entry{Method: "POST", Path: "/p'q", ReqHeaders: [][2]string{h.Header, {"Host", "x"}}, ReqBody: b}
			cmd := curlCommand(e, "https://shop.tuzy.dev")
			// Replace curl with a function that prints its argv (NUL-separated) and stdin.
			script := `curl() { for a in "$@"; do printf '%s\0' "$a"; done; printf 'STDIN:'; cat; }; ` + cmd
			out, err := exec.Command("sh", "-c", script).Output()
			if err != nil {
				t.Fatalf("sh: %v", err)
			}
			args := strings.Split(string(out), "\x00")
			if args[0] != "-X" || args[1] != "POST" || args[2] != "https://shop.tuzy.dev/p'q" {
				t.Fatalf("args %q", args[:3])
			}
			if !contains(args, h.Header[0]+": "+h.Header[1]) {
				t.Fatalf("header lost: %q", args)
			}
			stdin := string(out[bytes.LastIndex(out, []byte("STDIN:"))+len("STDIN:"):])
			if b[0] == 0 {
				if stdin != string(b) {
					t.Fatalf("binary body %q", stdin)
				}
			} else if !contains(args, string(b)) {
				t.Fatalf("text body lost: %q", args)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("command substitution executed")
			}
		}
	}
}

// TestCurlUsesDataRawAndHeadFlag is M3: an '@'-prefixed text body must go through --data-raw (never
// --data-binary, which treats a leading '@' as "read this file"), HEAD uses curl's -I rather than
// -X HEAD, and a truncated/evicted/incomplete body gets a leading warning comment.
func TestCurlUsesDataRawAndHeadFlag(t *testing.T) {
	e := &Entry{Method: "POST", Path: "/p", ReqBody: []byte("@evil-looking-but-literal-data")}
	cmd := curlCommand(e, "https://x")
	if !strings.Contains(cmd, "--data-raw") {
		t.Fatalf("expected --data-raw for an @-prefixed text body: %s", cmd)
	}
	if strings.Contains(cmd, "--data-binary '@evil") {
		t.Fatalf("must never pass an @-prefixed body to --data-binary: %s", cmd)
	}

	head := &Entry{Method: "HEAD", Path: "/h"}
	cmdHead := curlCommand(head, "https://x")
	if !strings.Contains(cmdHead, "curl -I ") {
		t.Fatalf("expected -I for HEAD: %s", cmdHead)
	}
	if strings.Contains(cmdHead, "-X 'HEAD'") || strings.Contains(cmdHead, "-X HEAD") {
		t.Fatalf("HEAD must not use -X HEAD: %s", cmdHead)
	}

	for _, e := range []*Entry{
		{Method: "GET", Path: "/g", ReqBody: []byte("partial"), ReqTrunc: true},
		{Method: "GET", Path: "/g", ReqBody: []byte("partial"), Evicted: true},
		{Method: "GET", Path: "/g", ReqBody: []byte("partial"), ReqIncomplete: true},
	} {
		cmd := curlCommand(e, "https://x")
		if !strings.HasPrefix(cmd, "# body truncated") {
			t.Fatalf("expected a truncated-body comment: %s", cmd)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestDecodeForDisplay(t *testing.T) {
	var gz, br bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte("hello gzip"))
	_ = zw.Close()
	bw := brotli.NewWriter(&br)
	_, _ = bw.Write([]byte("hello br"))
	_ = bw.Close()
	if out, enc, trunc, err := decodeForDisplay([][2]string{{"Content-Encoding", "gzip"}}, gz.Bytes()); err != nil || enc != "gzip" || trunc || string(out) != "hello gzip" {
		t.Fatalf("gzip: %q %v", out, err)
	}
	if out, _, trunc, err := decodeForDisplay([][2]string{{"content-encoding", "br"}}, br.Bytes()); err != nil || trunc || string(out) != "hello br" {
		t.Fatalf("br: %q %v", out, err)
	}
	if out, _, _, err := decodeForDisplay(nil, []byte("plain")); err != nil || out != nil {
		t.Fatal("identity must not decode")
	}
	if out, _, _, err := decodeForDisplay([][2]string{{"content-encoding", "gzip"}}, nil); err != nil || out != nil {
		t.Fatal("empty body must not be decoded (L3)")
	}
	var big bytes.Buffer
	zw2 := gzip.NewWriter(&big)
	_, _ = zw2.Write(bytes.Repeat([]byte("z"), MaxBodyCapture+10))
	_ = zw2.Close()
	out, _, trunc, err := decodeForDisplay([][2]string{{"content-encoding", "gzip"}}, big.Bytes())
	if err != nil || !trunc || len(out) != MaxBodyCapture {
		t.Fatalf("expected a truncated decode at the cap: trunc=%v len=%d err=%v", trunc, len(out), err)
	}
}

func TestSSEStreamsEvents(t *testing.T) {
	app := httptest.NewServer(http.NotFoundHandler())
	defer app.Close()
	srv, s := startServer(t, app)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL()+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	_, _ = rd.ReadString('\n') // ": connected"
	record(s, "shop", "GET", "/live", nil, nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		line, err := rd.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "event: created") {
			return
		}
	}
	t.Fatal("no created event")
}

// TestServerCloseIsFastWithAnOpenSSEConnection is M8: Close must not wait out its own Shutdown
// timeout just because a UI tab is still holding /api/events open.
func TestServerCloseIsFastWithAnOpenSSEConnection(t *testing.T) {
	app := httptest.NewServer(http.NotFoundHandler())
	defer app.Close()
	store := NewStore()
	srv, err := Listen("127.0.0.1:0", store, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL()+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	_, _ = rd.ReadString('\n') // ": connected" — the SSE handler is now blocked reading events

	start := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Close took %v with an open SSE connection (want well under the 2s Shutdown timeout)", d)
	}
}

// TestSSEDoesNotPublishPerChunk is H1: RequestBody/ResponseBody/WSMessage must not publish an
// event for every chunk/message — only Begin ("created"), Response and End ("updated") do. WS
// counters are folded into the entry (List/Get) even though they were never individually published.
func TestSSEDoesNotPublishPerChunk(t *testing.T) {
	s := NewStore()
	ch, cancel := s.Subscribe()
	defer cancel()

	r := s.Begin("a", protocol.ReqHeadMsg{Method: "POST", Path: "/", Kind: "ws"})
	if ev := <-ch; ev.Type != "created" {
		t.Fatalf("want created, got %s", ev.Type)
	}
	for i := 0; i < 50; i++ {
		r.RequestBody([]byte("x"))
		r.WSMessage(true)
		r.WSMessage(false)
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected event from body/WS chunks: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	id := r.(*streamRec).id
	sum := s.List(Filter{})[0]
	if sum.WSMsgsIn != 50 || sum.WSMsgsOut != 50 {
		t.Fatalf("List did not fold live WS counters: in=%d out=%d", sum.WSMsgsIn, sum.WSMsgsOut)
	}
	e, _ := s.Get(id)
	if e.WSMsgsIn != 50 || e.WSMsgsOut != 50 {
		t.Fatalf("Get did not fold live WS counters: in=%d out=%d", e.WSMsgsIn, e.WSMsgsOut)
	}

	r.Response(101, nil)
	if ev := <-ch; ev.Type != "updated" {
		t.Fatalf("want updated on Response, got %s", ev.Type)
	}
	r.End("")
	if ev := <-ch; ev.Type != "updated" {
		t.Fatalf("want updated on End, got %s", ev.Type)
	}
}

// TestSlowSubscriberIsDisconnected is H1: once a subscriber's 256-buffer channel is full, the store
// closes and removes it instead of dropping events forever, so the SSE handler returns and the
// browser's EventSource reconnects.
func TestSlowSubscriberIsDisconnected(t *testing.T) {
	s := NewStore()
	ch, cancel := s.Subscribe()
	defer cancel()
	for i := 0; i < 400; i++ { // created+updated per record: well past the 256-deep buffer
		record(s, "a", "GET", "/", nil, nil)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed: the disconnect happened
			}
		case <-deadline:
			t.Fatal("slow subscriber channel was never closed")
		}
	}
}

// TestKeydownGuardsPresent is M7 (H2, checked at the source level: no JS runtime here): the keydown
// handler must bail out on modifier keys and auto-repeat.
func TestKeydownGuardsPresent(t *testing.T) {
	js, err := uiFS.ReadFile("ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ev.repeat", "ev.metaKey", "ev.ctrlKey", "ev.altKey"} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("app.js keydown handler is missing the %q guard", want)
		}
	}
}

// TestRequestIncompleteFlag is M1: End() flags ReqIncomplete when RequestEnd (REQ_END on the wire)
// was never called, or the captured size falls short of a declared Content-Length — and keeps the
// "cancelled" error visible (it used to be silently dropped).
func TestRequestIncompleteFlag(t *testing.T) {
	s := NewStore()

	// A visitor abort: no RequestEnd call before End.
	r := s.Begin("a", protocol.ReqHeadMsg{Method: "POST", Path: "/up", Kind: "http"})
	r.RequestBody([]byte("partial"))
	r.End("cancelled")
	e, _ := s.Get(r.(*streamRec).id)
	if !e.ReqIncomplete {
		t.Fatal("expected ReqIncomplete when RequestEnd was never called")
	}
	if e.Error != "cancelled" {
		t.Fatalf(`expected Error "cancelled" to be kept, got %q`, e.Error)
	}

	// A declared Content-Length longer than what was actually captured.
	r2 := s.Begin("a", protocol.ReqHeadMsg{Method: "POST", Path: "/up", Kind: "http",
		Headers: []protocol.Header{{"content-length", "100"}}})
	r2.RequestBody([]byte("short"))
	r2.RequestEnd()
	r2.End("")
	e2, _ := s.Get(r2.(*streamRec).id)
	if !e2.ReqIncomplete {
		t.Fatal("expected ReqIncomplete when captured size < declared Content-Length")
	}

	// The normal case: RequestEnd called, size matches — complete.
	r3 := s.Begin("a", protocol.ReqHeadMsg{Method: "GET", Path: "/", Kind: "http"})
	r3.RequestEnd()
	r3.End("")
	e3, _ := s.Get(r3.(*streamRec).id)
	if e3.ReqIncomplete {
		t.Fatal("a normal request must not be flagged incomplete")
	}
}

// TestBodyHooksDoNotBlockUnderContention is M2: RequestBody/ResponseBody use TryLock, so a request
// concurrently holding the store lock never blocks another stream's capture — the contended call
// returns immediately and the drop is folded into ReqTrunc on the next successful update.
func TestBodyHooksDoNotBlockUnderContention(t *testing.T) {
	s := NewStore()
	r := s.Begin("a", protocol.ReqHeadMsg{Method: "POST", Path: "/", Kind: "http"})

	var wg sync.WaitGroup
	wg.Add(1)
	release := make(chan struct{})
	go func() {
		defer wg.Done()
		s.mu.Lock()
		<-release
		s.mu.Unlock()
	}()
	time.Sleep(20 * time.Millisecond) // ensure the goroutine above holds the lock

	done := make(chan struct{})
	go func() {
		r.RequestBody([]byte("dropped while contended"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RequestBody blocked on a contended store lock")
	}
	close(release)
	wg.Wait()

	// The dropped chunk should be folded in as a truncation on the next successful call.
	r.RequestBody([]byte("ok"))
	r.End("")
	e, _ := s.Get(r.(*streamRec).id)
	if !e.ReqTrunc {
		t.Fatal("a dropped chunk under contention must be folded in as ReqTrunc")
	}
}
