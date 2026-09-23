package tunnel

import (
	"bytes"

	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// Tests added for the phase-3 review (H1–H4, M1–M7) and kongming's advice.

func TestDrainFinishesLargeInFlightResponses(t *testing.T) {
	edge := newFakeEdge(t)
	body := bytes.Repeat([]byte("x"), 3<<20)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(body) }))
	defer app.Close()
	a := startAgent(t, edge.url(), app.URL, func(o *Options) { o.DrainGrace = 10 * time.Second })
	ec := edge.next(3 * time.Second)
	var streams []*edgeStream
	for i := 0; i < 4; i++ {
		streams = append(streams, ec.get(fmt.Sprintf("/d%d", i)))
	}
	time.Sleep(50 * time.Millisecond)
	a.cancel() // Ctrl-C while 12 MiB are in flight
	for _, s := range streams {
		_, got := s.response(t, 10*time.Second)
		if len(got) != len(body) {
			t.Fatalf("stream %d truncated: %d of %d", s.id, len(got), len(body))
		}
	}
	if code := ec.waitClosed(t, 5*time.Second); code != websocket.StatusNormalClosure {
		t.Fatalf("close code %d, want 1000", code)
	}
}

func TestHeartbeatFailsWhenNothingArrives(t *testing.T) {
	edge := newFakeEdge(t)
	edge.noPong = true
	startAgent(t, edge.url(), "http://127.0.0.1:1")
	first := edge.next(3 * time.Second)
	second := edge.next(3 * time.Second) // pong timeout (300 ms) → reconnect
	if second.hello.InstanceID != first.hello.InstanceID {
		t.Fatal("reconnect must reuse the instance id")
	}
}

func TestAnyInboundFrameCountsAsLiveness(t *testing.T) {
	edge := newFakeEdge(t)
	edge.noPong = true
	startAgent(t, edge.url(), "http://127.0.0.1:1")
	ec := edge.next(3 * time.Second)
	stop := make(chan struct{})
	defer close(stop)
	go func() { // the edge keeps talking (e.g. WINDOW frames during a big upload) but never pongs
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				ec.send(protocol.EncodeU32(protocol.Window, 0, 1))
			}
		}
	}()
	select {
	case <-edge.conns:
		t.Fatal("agent reconnected although frames kept arriving")
	case <-time.After(1500 * time.Millisecond):
	}
}

func TestProbeDetectsDeadPathQuickly(t *testing.T) {
	edge := newFakeEdge(t)
	var sess atomic.Pointer[session]
	startAgentHook(t, edge.url(), "http://127.0.0.1:1", func(c *Client) { c.onSession = func(s *session) { sess.Store(s) } }, func(o *Options) {
		o.PingInterval, o.PongTimeout, o.ProbeTimeout = time.Hour, time.Hour, 200*time.Millisecond
	})
	first := edge.next(3 * time.Second)
	// Force a reconnect so the hook captures a session, then go silent and simulate a wake-up.
	_ = first.c.CloseNow()
	edge.mu.Lock()
	edge.noPong = true
	edge.mu.Unlock()
	ec := edge.next(3 * time.Second)
	ec.mu.Lock()
	ec.noPong = true
	ec.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	sess.Load().probe <- struct{}{}
	edge.next(3 * time.Second)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("probe-triggered reconnect took %v", d)
	}
}

func TestGoawayBeforeReadyExits(t *testing.T) {
	edge := newFakeEdge(t)
	edge.goaway = &protocol.GoawayMsg{Reason: protocol.ReasonUpgradeRequired}
	a := startAgent(t, edge.url(), "http://127.0.0.1:1")
	var exit *ExitError
	if err := a.wait(t, 3*time.Second); !errors.As(err, &exit) || exit.Code != protocol.ReasonUpgradeRequired {
		t.Fatalf("err = %v", err)
	}
}

func TestCreditTimeoutResets(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(make([]byte, 3<<20)) }))
	defer app.Close()
	startAgent(t, edge.url(), app.URL, func(o *Options) { o.CreditTimeout = 500 * time.Millisecond })
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/big", "http")
	s.pause = true // the "visitor" never reads: no WINDOW comes back
	ec.send(protocol.Encode(protocol.ReqEnd, s.id, nil))
	select {
	case r := <-s.reset:
		if r.Code != protocol.ResetCreditTimeout {
			t.Fatalf("reset %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no credit_timeout RESET")
	}
}

func TestRetriesOn5xx(t *testing.T) {
	edge := newFakeEdge(t)
	edge.reject(502, `{"error":{"code":"bad_gateway"}}`, nil)
	startAgent(t, edge.url(), "http://127.0.0.1:1")
	time.Sleep(200 * time.Millisecond)
	edge.reject(0, "", nil)
	edge.next(5 * time.Second)
}

func TestResetMidStreamStopsTheBody(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < 1000; i++ {
			if _, err := w.Write(make([]byte, 64<<10)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	s := ec.get("/stream")
	<-s.head
	<-s.body
	b, _ := protocol.EncodeJSON(protocol.Reset, s.id, protocol.ResetMsg{Code: protocol.ResetCancelled})
	ec.send(b)
	time.Sleep(200 * time.Millisecond)
	for len(s.body) > 0 {
		<-s.body
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(s.body); n > 2 {
		t.Fatalf("agent kept sending %d chunks after RESET", n)
	}
}

func TestWebSocketToleranceCloses1008(t *testing.T) {
	edge := newFakeEdge(t)
	block := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		<-block // never reads
	}))
	defer app.Close()
	defer close(block)
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/ws", "ws")
	if h := <-s.head; h.Status != 101 {
		t.Fatalf("head %+v", h)
	}
	for i := 0; i < 40; i++ {
		ec.send(protocol.Encode(protocol.WSBinary, s.id, make([]byte, 256<<10))) // fills the local socket
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-s.ws:
			if f.Type == protocol.WSClose {
				if !strings.Contains(string(f.Payload), "1008") {
					t.Fatalf("close %s", f.Payload)
				}
				return
			}
		case <-deadline:
			t.Fatal("no WS_CLOSE 1008 after exceeding the tolerance")
		}
	}
}

func TestHeadAnd204HaveNoSyntheticLength(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/none" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Length", "42")
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	head, body := ec.get("/none").response(t, 3*time.Second)
	for _, h := range head.Headers {
		if strings.EqualFold(h[0], "content-length") {
			t.Fatalf("204 carries %s", h)
		}
	}
	if head.Status != 204 || len(body) != 0 {
		t.Fatalf("got %d %q", head.Status, body)
	}
	hs := ec.open("HEAD", "/h", "http")
	ec.send(protocol.Encode(protocol.ReqEnd, hs.id, nil))
	h2, b2 := hs.response(t, 3*time.Second)
	if h2.Status != 200 || len(b2) != 0 {
		t.Fatalf("HEAD got %d %d bytes", h2.Status, len(b2))
	}
}

func TestEarlyStreamingResponseDoesNotStallTheBody(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200) // streams a response and never reads the upload
		w.(http.Flusher).Flush()
		time.Sleep(4 * time.Second)
		io.WriteString(w, "done")
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL)
	ec := edge.next(3 * time.Second)
	data := make([]byte, 6<<20)
	s := ec.open("POST", "/up", "http", protocol.Header{"content-length", fmt.Sprint(len(data))})
	if err := ec.sendBody(s, data); err != nil { // must not hit the 5 s credit timeout
		t.Fatalf("edge body pump stalled: %v", err)
	}
	if _, body := s.response(t, 10*time.Second); string(body) != "done" {
		t.Fatalf("body %q", body)
	}
}

func TestErrorBodiesUseCredit(t *testing.T) {
	edge := newFakeEdge(t)
	startAgent(t, edge.url(), "http://127.0.0.1:1")
	ec := edge.next(3 * time.Second)
	for i := 0; i < 20; i++ {
		if h, _ := ec.get(fmt.Sprintf("/%d", i)).response(t, 3*time.Second); h.Status != 502 {
			t.Fatalf("status %d", h.Status)
		}
	}
	// The fake edge fails the test if the agent ever exceeds the connection window (held > window).
}

func TestNoGoroutineLeakAcrossReconnects(t *testing.T) {
	edge := newFakeEdge(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") }))
	defer app.Close()
	startAgentHook(t, edge.url(), app.URL, func(c *Client) { c.backoff = &backoff{rand: func() float64 { return 0 }} }) // reconnect instantly
	ec := edge.next(3 * time.Second)
	ec.get("/").response(t, 3*time.Second)
	runtime.GC()
	base := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		_ = ec.c.CloseNow()
		ec = edge.next(5 * time.Second)
		ec.get("/").response(t, 3*time.Second)
	}
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	if d := runtime.NumGoroutine() - base; d > 10 {
		t.Fatalf("goroutines grew by %d over 20 reconnects", d)
	}
}
