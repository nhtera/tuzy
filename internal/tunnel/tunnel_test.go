package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

type agentRun struct {
	client *Client
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returns
	err    error
}

func startAgent(t *testing.T, server *url.URL, target string, mut ...func(*Options)) *agentRun {
	t.Helper()
	return startAgentHook(t, server, target, nil, mut...)
}

// startAgentHook is startAgent with a hook that runs on the Client before Run starts.
func startAgentHook(t *testing.T, server *url.URL, target string, hook func(*Client), mut ...func(*Options)) *agentRun {
	t.Helper()
	tu, _ := url.Parse(target)
	opts := Options{
		Server: server, Token: "tok", Name: "shop", Target: tu, UserAgent: "tuzy/test",
		PingInterval: 200 * time.Millisecond, PongTimeout: 300 * time.Millisecond,
		CreditTimeout: 2 * time.Second, DrainGrace: 2 * time.Second, HandshakeTimeout: 2 * time.Second,
	}
	for _, m := range mut {
		m(&opts)
	}
	c, err := NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	if hook != nil {
		hook(c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &agentRun{client: c, cancel: cancel, done: make(chan struct{})}
	go func() {
		r.err = c.Run(ctx)
		close(r.done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return r
}

func (r *agentRun) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(timeout):
		t.Fatal("agent did not exit")
		return nil
	}
}

func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestHelloCarriesInstanceAndToken(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.NotFoundHandler())
	defer app.Close()
	a := startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	if ec.hello.InstanceID != a.client.InstanceID() || ec.query.Get("instance") != a.client.InstanceID() {
		t.Fatalf("instance mismatch: hello %q query %q", ec.hello.InstanceID, ec.query.Get("instance"))
	}
	if ec.auth != "Bearer tok" || ec.hello.Proto != 1 || ec.query.Get("name") != "shop" {
		t.Fatalf("bad connect: auth %q hello %+v", ec.auth, ec.hello)
	}
}

func TestGETRelayPreservesHostAndHeaders(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		fmt.Fprintf(w, "%s %s host=%s xff=%s", r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("X-Forwarded-For"))
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/a%2Fb/c?x=1", "http", protocol.Header{"x-forwarded-for", "203.0.113.9"})
	ec.send(protocol.Encode(protocol.ReqEnd, s.id, nil))
	head, body := s.response(t, 3*time.Second)
	if head.Status != 200 || string(body) != "GET /a%2Fb/c?x=1 host=shop.tuzy.test xff=203.0.113.9" {
		t.Fatalf("got %d %q", head.Status, body)
	}
	cookies := 0
	for _, h := range head.Headers {
		if strings.EqualFold(h[0], "set-cookie") {
			cookies++
		}
	}
	if cookies != 2 {
		t.Fatalf("set-cookie pairs = %d, want 2", cookies)
	}
}

func TestHostHeaderRewrite(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Host) }))
	defer app.Close()
	startAgent(t, edge.url(), app.URL, func(o *Options) { o.HostHeader = "rewrite" })
	ec := edge.next(3 * time.Second)
	_, body := ec.get("/").response(t, 3*time.Second)
	if string(body) != strings.TrimPrefix(app.URL, "http://") {
		t.Fatalf("host = %q", body)
	}
}

func TestPOST20MBWithinCredit(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		io.WriteString(w, hash(b))
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	data := bytes.Repeat([]byte("tuzy-0123456789"), 20*1024*1024/15)
	s := ec.open("POST", "/upload", "http", protocol.Header{"content-length", fmt.Sprint(len(data))})
	if err := ec.sendBody(s, data); err != nil {
		t.Fatalf("send body: %v", err)
	}
	_, body := s.response(t, 20*time.Second)
	if string(body) != hash(data) {
		t.Fatal("checksum mismatch")
	}
}

func TestLargeDownloadRespectsEdgeCredit(t *testing.T) {
	edge := newFakeEdge(t)
	data := bytes.Repeat([]byte{0xab}, 12*1024*1024)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(data) }))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	_, body := ec.get("/big").response(t, 20*time.Second)
	if !bytes.Equal(body, data) {
		t.Fatalf("download mismatch: %d bytes", len(body))
	}
}

func TestSSEStreamsIncrementally(t *testing.T) {
	edge := newFakeEdge(t)
	next := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-next
		io.WriteString(w, "data: two\n\n")
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	s := ec.get("/events")
	<-s.head
	select {
	case b := <-s.body:
		if string(b) != "data: one\n\n" {
			t.Fatalf("first chunk %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE chunk not relayed before the response ended")
	}
	close(next)
	select {
	case b := <-s.body:
		if string(b) != "data: two\n\n" {
			t.Fatalf("second chunk %q", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second chunk missing")
	}
}

func TestWebSocketEcho(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"chat"}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			typ, msg, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), typ, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/ws", "ws", protocol.Header{"sec-websocket-protocol", "chat"})
	head := <-s.head
	if head.Status != 101 || len(head.Headers) != 1 || head.Headers[0][1] != "chat" {
		t.Fatalf("head = %+v", head)
	}
	ec.send(protocol.Encode(protocol.WSText, s.id, []byte("hi")))
	var gotAck, gotEcho bool
	for !gotAck || !gotEcho {
		select {
		case f := <-s.ws:
			switch f.Type {
			case protocol.Ack:
				gotAck = true
			case protocol.WSText:
				if string(f.Payload) != "echo:hi" {
					t.Fatalf("echo %q", f.Payload)
				}
				gotEcho = true
				ec.send(protocol.EncodeU32(protocol.Ack, s.id, 1))
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("ack=%v echo=%v", gotAck, gotEcho)
		}
	}
	// Visitor closes → local socket closes → agent answers WS_CLOSE.
	b, _ := protocol.EncodeJSON(protocol.WSClose, s.id, protocol.WSCloseMsg{Code: 4000, Reason: "bye"})
	ec.send(b)
	for {
		select {
		case f := <-s.ws:
			if f.Type == protocol.WSClose {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no WS_CLOSE answer")
		}
	}
}

func TestSlowReaderDoesNotStallOtherStreamsOrHeartbeat(t *testing.T) {
	edge := newFakeEdge(t)
	block := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stuck" {
			<-block // never reads its body
			return
		}
		io.WriteString(w, "fast")
	}))
	defer app.Close()
	defer close(block) // runs first: unblock the handler so app.Close can finish
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)

	stuck := ec.open("POST", "/stuck", "http", protocol.Header{"content-length", fmt.Sprint(8 << 20)})
	go func() { _ = ec.sendBody(stuck, make([]byte, 8<<20)) }() // stalls on credit; must not block the agent
	time.Sleep(300 * time.Millisecond)
	ec.mu.Lock()
	pingsBefore := ec.pings
	ec.mu.Unlock()

	_, body := ec.get("/fast").response(t, 3*time.Second)
	if string(body) != "fast" {
		t.Fatalf("fast stream got %q", body)
	}
	time.Sleep(600 * time.Millisecond)
	ec.mu.Lock()
	pingsAfter := ec.pings
	ec.mu.Unlock()
	if pingsAfter <= pingsBefore {
		t.Fatal("heartbeat stopped while a stream was stalled")
	}
}

func TestEarlyResponseKeepsGrantingCredit(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge) // without reading the body
		io.WriteString(w, "too large")
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	data := make([]byte, 12<<20)
	s := ec.open("POST", "/upload", "http", protocol.Header{"content-length", fmt.Sprint(len(data))})
	sent := make(chan error, 1)
	go func() { sent <- ec.sendBody(s, data) }()
	head, body := s.response(t, 5*time.Second)
	if head.Status != 413 || string(body) != "too large" {
		t.Fatalf("got %d %q", head.Status, body)
	}
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("edge body pump failed (credit deadlock before RES_END?): %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("edge body pump stalled after early response")
	}
	// Everything the edge sent — consumed, discarded or late — comes back as connection credit.
	deadline := time.Now().Add(3 * time.Second)
	for {
		ec.conn.mu.Lock()
		avail := ec.conn.avail
		ec.conn.mu.Unlock()
		if avail == protocol.ConnWindow {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection credit leaked: %d of %d", avail, protocol.ConnWindow)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLocalUnreachableIs502(t *testing.T) {
	edge := newFakeEdge(t)
	startAgent(t, edge.url(), "http://127.0.0.1:1") // nothing listens on port 1
	ec := edge.next(3 * time.Second)
	head, body := ec.get("/").response(t, 5*time.Second)
	if head.Status != 502 || !strings.Contains(string(body), "could not reach") {
		t.Fatalf("got %d %q", head.Status, body)
	}
}

func TestHalfOpenReconnectsWithSameInstance(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer app.Close()
	proxy := newBlackholeProxy(t, edge.url().Host)
	pu, _ := url.Parse("http://" + proxy.addr())
	a := startAgent(t, pu, app.URL)
	first := edge.next(3 * time.Second)

	proxy.blackhole() // the path goes silent: no FIN, no RST
	start := time.Now()
	second := edge.next(5 * time.Second)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("reconnect took %v", d)
	}
	if second.hello.InstanceID != first.hello.InstanceID || second.hello.InstanceID != a.client.InstanceID() {
		t.Fatal("reconnect must reuse the instance id")
	}
	_, body := second.get("/").response(t, 3*time.Second)
	if string(body) != "ok" {
		t.Fatalf("after reconnect got %q", body)
	}
}

func TestStatusTable(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{400, `{"error":{"code":"invalid_name","message":"invalid tunnel name"}}`, "invalid tunnel name"},
		{401, `{"error":{"code":"unauthorized"}}`, "not logged in: run `tuzy login`"},
		{401, `{"error":{"code":"token_expired"}}`, "session expired: run `tuzy login`"},
		{403, `{"error":{"code":"forbidden","message":"not your name"}}`, "not your name"},
		{404, `{"error":{"code":"name_not_reserved"}}`, "you don't own `shop`: run `tuzy names add shop`"},
		{409, `{"error":{"code":"name_in_use"}}`, "`shop` is already live in another tuzy process"},
		{409, `{"error":{"code":"name_in_use"}}`, "--name <other>"},
		{409, `{"error":{"code":"name_in_use"}}`, "rerun with --force"},
		{410, `{"error":{"code":"name_released","new_name":"store"}}`, "was renamed to `store`"}, // hint loop: gives up after 3 hops
		{410, `{"error":{"code":"name_released"}}`, "`shop` was removed"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			edge := newFakeEdge(t)
			edge.reject(tc.status, tc.body, nil)
			a := startAgent(t, edge.url(), "http://127.0.0.1:1")
			err := a.wait(t, 5*time.Second)
			var exit *ExitError
			if !errors.As(err, &exit) || !strings.Contains(exit.Message, tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestRetriesOn429ThenConnects(t *testing.T) {
	edge := newFakeEdge(t)
	edge.reject(429, `{"error":{"code":"rate_limited"}}`, http.Header{"Retry-After": {"1"}})
	var reconnecting atomic.Int32
	startAgent(t, edge.url(), "http://127.0.0.1:1", func(o *Options) {
		o.OnEvent = func(e Event) {
			if e.Kind == EventReconnecting {
				reconnecting.Add(1)
			}
		}
	})
	time.Sleep(300 * time.Millisecond)
	edge.reject(0, "", nil)
	edge.next(5 * time.Second)
	if reconnecting.Load() == 0 {
		t.Fatal("expected a retry after 429")
	}
}

func TestGoawayRenamedReconnectsAsNewName(t *testing.T) {
	edge := newFakeEdge(t)
	a := startAgent(t, edge.url(), "http://127.0.0.1:1")
	ec := edge.next(3 * time.Second)
	b, _ := protocol.EncodeJSON(protocol.Goaway, 0, protocol.GoawayMsg{Reason: protocol.ReasonRenamed, NewName: "store"})
	ec.send(b)
	_ = ec.c.Close(websocket.StatusNormalClosure, "renamed")
	next := edge.next(3 * time.Second)
	if next.query.Get("name") != "store" || a.client.Name() != "store" {
		t.Fatalf("reconnected as %q", next.query.Get("name"))
	}
}

func TestGoneWithRenameHintFollowsNewName(t *testing.T) {
	edge := newFakeEdge(t)
	edge.reject(410, `{"error":{"code":"name_released","new_name":"store"}}`, nil)
	edge.mu.Lock()
	edge.onlyName = "shop"
	edge.mu.Unlock()
	var renamed atomic.Int32
	a := startAgent(t, edge.url(), "http://127.0.0.1:1", func(o *Options) {
		o.OnEvent = func(e Event) {
			if e.Kind == EventRenamed && e.OldName == "shop" && e.Name == "store" {
				renamed.Add(1)
			}
		}
	})
	next := edge.next(3 * time.Second)
	if next.query.Get("name") != "store" || a.client.Name() != "store" || renamed.Load() != 1 {
		t.Fatalf("connected as %q (renamed events %d)", next.query.Get("name"), renamed.Load())
	}
}

func TestGoawayTerminalReasonsExit(t *testing.T) {
	for _, reason := range []string{protocol.ReasonReplaced, protocol.ReasonSuspended, protocol.ReasonRevoked, protocol.ReasonDeleted, protocol.ReasonUpgradeRequired} {
		t.Run(reason, func(t *testing.T) {
			edge := newFakeEdge(t)
			a := startAgent(t, edge.url(), "http://127.0.0.1:1")
			ec := edge.next(3 * time.Second)
			b, _ := protocol.EncodeJSON(protocol.Goaway, 0, protocol.GoawayMsg{Reason: reason})
			ec.send(b)
			_ = ec.c.Close(websocket.StatusNormalClosure, reason)
			var exit *ExitError
			if err := a.wait(t, 3*time.Second); !errors.As(err, &exit) || exit.Code != reason {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestDrainOnCancelFinishesInFlight(t *testing.T) {
	edge := newFakeEdge(t)
	release := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		io.WriteString(w, "finished")
	}))
	defer app.Close()
	a := startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	s := ec.get("/slow")
	time.Sleep(100 * time.Millisecond)
	a.cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		ec.mu.Lock()
		drained := ec.drained
		ec.mu.Unlock()
		if drained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no DRAIN")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	_, body := s.response(t, 3*time.Second)
	if string(body) != "finished" {
		t.Fatalf("in-flight got %q", body)
	}
	if code := ec.waitClosed(t, 3*time.Second); code != websocket.StatusNormalClosure {
		t.Fatalf("close code %d", code)
	}
	if err := a.wait(t, 3*time.Second); err != nil {
		t.Fatalf("drain returned %v", err)
	}
}

func TestNonIncreasingStreamIDCloses1002(t *testing.T) {
	edge := newFakeEdge(t)
	startAgent(t, edge.url(), "http://127.0.0.1:1")
	ec := edge.next(3 * time.Second)
	b, _ := protocol.EncodeJSON(protocol.ReqHead, 5, protocol.ReqHeadMsg{Method: "GET", Path: "/", Kind: "http"})
	ec.send(b)
	ec.send(b) // same id again
	if code := ec.waitClosed(t, 3*time.Second); code != websocket.StatusProtocolError {
		t.Fatalf("close code %d", code)
	}
}

func TestBackoffBounds(t *testing.T) {
	b := &backoff{rand: func() float64 { return 0.999 }}
	if d := b.delay(0); d > backoffBase {
		t.Fatalf("attempt 0 delay %v", d)
	}
	if d := b.delay(100); d > backoffMax {
		t.Fatalf("capped delay %v", d)
	}
	if d := b.restart(); d > restartJitter {
		t.Fatalf("restart jitter %v", d)
	}
	now := time.Now()
	if d := retryAfter(http.Header{"Retry-After": {"7"}}, now); d != 7*time.Second {
		t.Fatalf("retry-after %v", d)
	}
}

// The edge resets a tunnel object that never received HELLO; the agent retries promptly (restart
// jitter) instead of growing its backoff, so a wedged object costs seconds, not minutes.
func TestNoHelloRetriesPromptly(t *testing.T) {
	edge := newFakeEdge(t)
	edge.mu.Lock()
	edge.noHello = 6
	edge.mu.Unlock()
	var mu sync.Mutex
	var waits []time.Duration
	a := startAgentHook(t, edge.url(), "http://127.0.0.1:1", func(c *Client) {
		c.backoff = &backoff{rand: func() float64 { return 0.2 }} // restart 0.6 s; exponential would reach 1.6 s
	}, func(o *Options) {
		o.OnEvent = func(e Event) {
			if e.Kind == EventReconnecting {
				mu.Lock()
				waits = append(waits, e.RetryIn)
				mu.Unlock()
			}
		}
	})
	edge.next(15 * time.Second) // online after the six "no HELLO" closes
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 6 {
		t.Fatalf("reconnect events = %d, want 6", len(waits))
	}
	for i, w := range waits {
		if w > time.Duration(0.2*float64(restartJitter)) {
			t.Fatalf("retry %d waited %s, want restart jitter (≤ %s)", i+1, w, time.Duration(0.2*float64(restartJitter)))
		}
	}
	_ = a
}
