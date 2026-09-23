package tunnel

// Tests for the phase-6 review (H1/M1/M2/M6/M7): a real (non-nop) Recorder driven through the fake
// edge, verifying the tunnel-side contract the inspector relies on — body parity in both
// directions, WebSocket message counts, and the RequestEnd hook that flags an incomplete request.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// fakeRecorder is a minimal real Recorder (not the inspector's — importing internal/inspector here
// would cycle, since it imports tunnel) that captures exactly what StreamRecorder exposes.
type fakeRecorder struct {
	mu      sync.Mutex
	entries map[uint64]*fakeEntry
	next    uint64
}

type fakeEntry struct {
	reqBody, resBody []byte
	wsIn, wsOut      int
	status           int
	err              string
	reqEndCalled     bool
	ended            bool
}

func newFakeRecorder() *fakeRecorder { return &fakeRecorder{entries: map[uint64]*fakeEntry{}} }

func (r *fakeRecorder) Begin(_ string, _ protocol.ReqHeadMsg) StreamRecorder {
	r.mu.Lock()
	r.next++
	id := r.next
	r.entries[id] = &fakeEntry{}
	r.mu.Unlock()
	return &fakeStreamRec{r: r, id: id}
}

// get waits (bounded) for id's entry to exist and reach End, then returns a copy. Begin() lands
// asynchronously (it's dispatched by the agent's read loop after the REQ_HEAD frame crosses the
// fake network), so the entry may not exist yet on the first check.
func (r *fakeRecorder) get(t *testing.T, id uint64, timeout time.Duration) fakeEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		e, ok := r.entries[id]
		var snap fakeEntry
		if ok {
			snap = *e
		}
		r.mu.Unlock()
		if (ok && snap.ended) || time.Now().After(deadline) {
			return snap
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type fakeStreamRec struct {
	r  *fakeRecorder
	id uint64
}

func (s *fakeStreamRec) RequestBody(p []byte) {
	s.r.mu.Lock()
	s.r.entries[s.id].reqBody = append(s.r.entries[s.id].reqBody, p...)
	s.r.mu.Unlock()
}

func (s *fakeStreamRec) RequestEnd() {
	s.r.mu.Lock()
	s.r.entries[s.id].reqEndCalled = true
	s.r.mu.Unlock()
}

func (s *fakeStreamRec) Response(status int, _ []protocol.Header) {
	s.r.mu.Lock()
	s.r.entries[s.id].status = status
	s.r.mu.Unlock()
}

func (s *fakeStreamRec) ResponseBody(p []byte) {
	s.r.mu.Lock()
	s.r.entries[s.id].resBody = append(s.r.entries[s.id].resBody, p...)
	s.r.mu.Unlock()
}

func (s *fakeStreamRec) WSMessage(fromVisitor bool) {
	s.r.mu.Lock()
	if fromVisitor {
		s.r.entries[s.id].wsIn++
	} else {
		s.r.entries[s.id].wsOut++
	}
	s.r.mu.Unlock()
}

func (s *fakeStreamRec) End(errMsg string) {
	s.r.mu.Lock()
	s.r.entries[s.id].ended = true
	s.r.entries[s.id].err = errMsg
	s.r.mu.Unlock()
}

// TestRecorderCapturesHTTPBodyParityBothWays is M7: the recorder must see exactly the bytes the
// edge sent (request) and exactly the bytes the local app sent back (response), independent of
// what the visitor/local app actually consumed.
func TestRecorderCapturesHTTPBodyParityBothWays(t *testing.T) {
	edge := newFakeEdge(t)
	rec := newFakeRecorder()
	reqBody := bytes.Repeat([]byte("Q"), 5000)
	resBody := bytes.Repeat([]byte("A"), 7000)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, reqBody) {
			t.Errorf("local app got a different body: %d bytes", len(got))
		}
		_, _ = w.Write(resBody)
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL, func(o *Options) { o.Recorder = rec })
	ec := edge.next(3 * time.Second)
	s := ec.open("POST", "/echo", "http", protocol.Header{"content-length", fmt.Sprint(len(reqBody))})
	if err := ec.sendBody(s, reqBody); err != nil {
		t.Fatalf("sendBody: %v", err)
	}
	_, body := s.response(t, 3*time.Second)
	if !bytes.Equal(body, resBody) {
		t.Fatalf("visitor got a different body: %d bytes", len(body))
	}

	e := rec.get(t, 1, 3*time.Second)
	if !e.ended {
		t.Fatal("recorder.End was never called")
	}
	if !bytes.Equal(e.reqBody, reqBody) {
		t.Fatalf("recorder captured a different request body: got %d want %d bytes", len(e.reqBody), len(reqBody))
	}
	if !bytes.Equal(e.resBody, resBody) {
		t.Fatalf("recorder captured a different response body: got %d want %d bytes", len(e.resBody), len(resBody))
	}
	if !e.reqEndCalled {
		t.Fatal("RequestEnd was never called for a complete request")
	}
	if e.status != 200 {
		t.Fatalf("status = %d", e.status)
	}
}

// TestRecorderCountsWSMessages is M7: the recorder's WSMessage hook must see every message in both
// directions.
func TestRecorderCountsWSMessages(t *testing.T) {
	edge := newFakeEdge(t)
	rec := newFakeRecorder()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for i := 0; i < 3; i++ {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if err := c.Write(r.Context(), websocket.MessageText, data); err != nil {
				return
			}
		}
		<-r.Context().Done()
	}))
	defer app.Close()
	startAgent(t, edge.url(), app.URL, func(o *Options) { o.Recorder = rec })
	ec := edge.next(3 * time.Second)
	s := ec.open("GET", "/ws", "ws")
	if h := <-s.head; h.Status != 101 {
		t.Fatalf("head %+v", h)
	}
	for i := 0; i < 3; i++ {
		ec.send(protocol.Encode(protocol.WSText, s.id, []byte("hi")))
	}
	// Each sent message yields both an ACK (edge→agent delivery) and an echoed WS_TEXT
	// (agent→edge, from the local app), interleaved in either order: collect 3 of the latter.
	echoed := 0
	deadlineWS := time.After(3 * time.Second)
	for echoed < 3 {
		select {
		case f := <-s.ws:
			if f.Type == protocol.WSText {
				echoed++
			} else if f.Type != protocol.Ack {
				t.Fatalf("unexpected frame %s", f.Type)
			}
		case <-deadlineWS:
			t.Fatalf("timed out waiting for the echoed messages (got %d of 3)", echoed)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	var e fakeEntry
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		e = *rec.entries[1]
		rec.mu.Unlock()
		if e.wsIn == 3 && e.wsOut == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.wsIn != 3 || e.wsOut != 3 {
		t.Fatalf("ws counts: in=%d out=%d", e.wsIn, e.wsOut)
	}
}

// TestRecorderRequestEndReflectsAnAbortedUpload is M7 (M1's mechanism): RequestEnd must fire when
// REQ_END is seen, and must NOT fire when the stream is reset before REQ_END (a visitor abort) —
// this is exactly what the inspector uses to flag an incomplete request body. "cancelled" must
// still reach End's errMsg.
func TestRecorderRequestEndReflectsAnAbortedUpload(t *testing.T) {
	edge := newFakeEdge(t)
	rec := newFakeRecorder()
	block := make(chan struct{})
	app := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block // never reads the body, never responds
	}))
	defer app.Close()
	defer close(block)
	startAgent(t, edge.url(), app.URL, func(o *Options) { o.Recorder = rec })
	ec := edge.next(3 * time.Second)
	s := ec.open("POST", "/up", "http", protocol.Header{"content-length", "100"})
	ec.send(protocol.Encode(protocol.ReqBody, s.id, []byte("partial")))
	// Reset the stream before REQ_END is ever sent — the visitor aborted mid-upload.
	b, _ := protocol.EncodeJSON(protocol.Reset, s.id, protocol.ResetMsg{Code: protocol.ResetCancelled})
	ec.send(b)

	e := rec.get(t, 1, 3*time.Second)
	if !e.ended {
		t.Fatal("recorder.End was never called")
	}
	if e.reqEndCalled {
		t.Fatal("RequestEnd must not be called when the stream is reset before REQ_END")
	}
	if e.err != "cancelled" {
		t.Fatalf(`expected End("cancelled"), got %q`, e.err)
	}
}

// TestRecorderRequestEndOnNormalRequest is the complement: a normal (complete) request, including
// one with no body at all, always sees RequestEnd before End.
func TestRecorderRequestEndOnNormalRequest(t *testing.T) {
	edge := newFakeEdge(t)
	rec := newFakeRecorder()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer app.Close()
	startAgent(t, edge.url(), app.URL, func(o *Options) { o.Recorder = rec })
	ec := edge.next(3 * time.Second)
	ec.get("/none").response(t, 3*time.Second)

	e := rec.get(t, 1, 3*time.Second)
	if !e.ended {
		t.Fatal("recorder.End was never called")
	}
	if !e.reqEndCalled {
		t.Fatal("RequestEnd must be called for a normal (bodyless) request")
	}
}
