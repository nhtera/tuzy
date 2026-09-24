package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// fakeEdge speaks the edge side of wire protocol v1 for agent tests.
type fakeEdge struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	status   int    // when non-zero, connects are rejected with this status…
	body     string // …and this JSON body
	header   http.Header
	onlyName string // when set, only connects for this name are rejected
	noPong   bool
	goaway   *protocol.GoawayMsg // sent instead of READY
	noHello  int                 // the next N connects are closed 1002 "no HELLO" (a wedged tunnel object)
	conns    chan *edgeConn
}

func newFakeEdge(t *testing.T) *fakeEdge {
	e := &fakeEdge{t: t, conns: make(chan *edgeConn, 16)}
	e.srv = httptest.NewServer(http.HandlerFunc(e.handle))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *fakeEdge) url() *url.URL {
	u, _ := url.Parse(e.srv.URL)
	return u
}

func (e *fakeEdge) reject(status int, body string, h http.Header) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status, e.body, e.header = status, body, h
}

func (e *fakeEdge) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/connect" {
		http.NotFound(w, r)
		return
	}
	e.mu.Lock()
	status, body, header, goaway, noPong := e.status, e.body, e.header, e.goaway, e.noPong
	if e.onlyName != "" && r.URL.Query().Get("name") != e.onlyName {
		status = 0
	}
	e.mu.Unlock()
	if status != 0 {
		for k, v := range header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(4 << 20)
	ec := &edgeConn{
		t: e.t, c: c, query: r.URL.Query(), auth: r.Header.Get("Authorization"),
		frames:  make(chan protocol.Frame, 4096),
		streams: map[uint32]*edgeStream{},
		conn:    newCredit(protocol.ConnWindow),
		noPong:  noPong,
		closed:  make(chan struct{}),
	}
	ctx := context.Background()
	_, msg, err := c.Read(ctx)
	if err != nil {
		return
	}
	f, err := protocol.Decode(msg)
	if err != nil || f.Type != protocol.Hello {
		_ = c.Close(websocket.StatusProtocolError, "expected HELLO")
		return
	}
	_ = json.Unmarshal(f.Payload, &ec.hello)
	e.mu.Lock()
	wedged := e.noHello > 0
	if wedged {
		e.noHello--
	}
	e.mu.Unlock()
	if wedged {
		_ = c.Close(websocket.StatusProtocolError, "no HELLO")
		return
	}
	if goaway != nil {
		b, _ := protocol.EncodeJSON(protocol.Goaway, 0, goaway)
		_ = c.Write(ctx, websocket.MessageBinary, b)
		_ = c.Close(websocket.StatusNormalClosure, "goaway")
		return
	}
	b, _ := protocol.EncodeJSON(protocol.Ready, 0, protocol.ReadyMsg{Name: ec.query.Get("name"), URL: "https://" + ec.query.Get("name") + ".tuzy.test", Epoch: 1})
	_ = c.Write(ctx, websocket.MessageBinary, b)
	go ec.readLoop()
	e.conns <- ec
	<-ec.closed
}

// next waits for the agent's next connection.
func (e *fakeEdge) next(timeout time.Duration) *edgeConn {
	e.t.Helper()
	select {
	case ec := <-e.conns:
		return ec
	case <-time.After(timeout):
		e.t.Fatal("timed out waiting for agent connection")
		return nil
	}
}

// edgeConn is one accepted agent socket.
type edgeConn struct {
	t      *testing.T
	c      *websocket.Conn
	query  url.Values
	auth   string
	hello  protocol.HelloMsg
	noPong bool
	frames chan protocol.Frame // every frame from the agent (for assertions)
	closed chan struct{}

	mu        sync.Mutex
	nextID    uint32
	streams   map[uint32]*edgeStream
	conn      *credit // edge → agent request-body connection credit
	pings     int
	drained   bool
	closeCode websocket.StatusCode
	held      int64 // agent → edge response bytes not yet granted (connection receive window)
}

// edgeStream is one stream as seen from the edge.
type edgeStream struct {
	id      uint32
	send    *credit // edge → agent request-body stream credit
	head    chan protocol.ResHeadMsg
	body    chan []byte
	end     chan struct{}
	reset   chan protocol.ResetMsg
	ws      chan protocol.Frame // WS_TEXT / WS_BINARY / WS_CLOSE / ACK
	pause   bool                // don't grant response credit (slow visitor)
	held    int64
	endOnce sync.Once
}

func (ec *edgeConn) send(b []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ec.c.Write(ctx, websocket.MessageBinary, b)
}

func (ec *edgeConn) readLoop() {
	defer close(ec.closed)
	for {
		typ, msg, err := ec.c.Read(context.Background())
		if err != nil {
			ec.mu.Lock()
			ec.closeCode = websocket.CloseStatus(err)
			ec.mu.Unlock()
			return
		}
		if typ == websocket.MessageText {
			ec.mu.Lock()
			ec.pings++
			noPong := ec.noPong
			ec.mu.Unlock()
			if string(msg) == protocol.Ping && !noPong {
				_ = ec.c.Write(context.Background(), websocket.MessageText, []byte(protocol.Pong))
			}
			continue
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			ec.t.Errorf("agent sent malformed frame: %v", err)
			return
		}
		ec.frames <- f
		ec.handle(f)
	}
}

func (ec *edgeConn) stream(id uint32) *edgeStream {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return ec.streams[id]
}

func (ec *edgeConn) handle(f protocol.Frame) {
	switch f.Type {
	case protocol.Drain:
		ec.mu.Lock()
		ec.drained = true
		ec.mu.Unlock()
		return
	case protocol.Window:
		if f.StreamID == 0 {
			_ = ec.conn.add(int64(f.Value))
		} else if s := ec.stream(f.StreamID); s != nil {
			_ = s.send.add(int64(f.Value))
		}
		return
	}
	s := ec.stream(f.StreamID)
	if s == nil {
		return
	}
	switch f.Type {
	case protocol.ResHead:
		var h protocol.ResHeadMsg
		_ = json.Unmarshal(f.Payload, &h)
		s.head <- h
	case protocol.ResBody:
		ec.mu.Lock()
		ec.held += int64(len(f.Payload))
		s.held += int64(len(f.Payload))
		if ec.held > protocol.ConnWindow || s.held > protocol.StreamWindow {
			ec.t.Errorf("agent exceeded credit: conn %d stream %d", ec.held, s.held)
		}
		pause := s.pause
		ec.mu.Unlock()
		s.body <- f.Payload
		if !pause {
			ec.grant(s, len(f.Payload))
		}
	case protocol.ResEnd:
		s.endOnce.Do(func() { close(s.end) })
	case protocol.Reset:
		var r protocol.ResetMsg
		_ = json.Unmarshal(f.Payload, &r)
		s.reset <- r
	case protocol.WSText, protocol.WSBinary, protocol.WSClose, protocol.Ack:
		s.ws <- f
	}
}

// grant returns response credit as the "visitor" consumes.
func (ec *edgeConn) grant(s *edgeStream, n int) {
	ec.mu.Lock()
	ec.held -= int64(n)
	s.held -= int64(n)
	ec.mu.Unlock()
	ec.send(protocol.EncodeU32(protocol.Window, s.id, uint32(n)))
	ec.send(protocol.EncodeU32(protocol.Window, 0, uint32(n)))
}

// open sends REQ_HEAD and returns the stream.
func (ec *edgeConn) open(method, path, kind string, headers ...protocol.Header) *edgeStream {
	ec.mu.Lock()
	ec.nextID++
	s := &edgeStream{
		id: ec.nextID, send: newCredit(protocol.StreamWindow),
		head: make(chan protocol.ResHeadMsg, 1), body: make(chan []byte, 4096), end: make(chan struct{}),
		reset: make(chan protocol.ResetMsg, 4), ws: make(chan protocol.Frame, 256),
	}
	ec.streams[s.id] = s
	ec.mu.Unlock()
	hs := append([]protocol.Header{{"host", "shop.tuzy.test"}}, headers...)
	b, _ := protocol.EncodeJSON(protocol.ReqHead, s.id, protocol.ReqHeadMsg{Method: method, Path: path, Headers: hs, RemoteIP: "203.0.113.9", Kind: kind})
	ec.send(b)
	return s
}

// sendBody pumps a request body under the agent's credit, then REQ_END. Like the real edge, it
// stops pumping as soon as the response has ended (PROTOCOL.md §4.3).
func (ec *edgeConn) sendBody(s *edgeStream, body []byte) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.end:
			cancel()
		case <-ctx.Done():
		}
	}()
	for off := 0; off < len(body); {
		n, err := acquire(ctx, s.send, ec.conn, min(protocol.MaxBodyChunk, len(body)-off), 5*time.Second)
		if errors.Is(err, context.Canceled) {
			break // RES_END: stop pumping, send REQ_END
		}
		if err != nil {
			return err
		}
		ec.send(protocol.Encode(protocol.ReqBody, s.id, body[off:off+n]))
		off += n
	}
	ec.send(protocol.Encode(protocol.ReqEnd, s.id, nil))
	return nil
}

func (ec *edgeConn) get(path string) *edgeStream {
	s := ec.open("GET", path, "http")
	ec.send(protocol.Encode(protocol.ReqEnd, s.id, nil))
	return s
}

// response waits for RES_HEAD and the full body.
func (s *edgeStream) response(t *testing.T, timeout time.Duration) (protocol.ResHeadMsg, []byte) {
	t.Helper()
	deadline := time.After(timeout)
	var head protocol.ResHeadMsg
	select {
	case head = <-s.head:
	case r := <-s.reset:
		t.Fatalf("stream %d reset: %+v", s.id, r)
	case <-deadline:
		t.Fatalf("stream %d: no RES_HEAD", s.id)
	}
	var body []byte
	for {
		select {
		case b := <-s.body:
			body = append(body, b...)
		case <-s.end:
			for {
				select {
				case b := <-s.body:
					body = append(body, b...)
				default:
					return head, body
				}
			}
		case <-deadline:
			t.Fatalf("stream %d: no RES_END (got %d bytes)", s.id, len(body))
		}
	}
}

func (ec *edgeConn) waitClosed(t *testing.T, timeout time.Duration) websocket.StatusCode {
	t.Helper()
	select {
	case <-ec.closed:
	case <-time.After(timeout):
		t.Fatal("agent connection not closed")
	}
	ec.mu.Lock()
	defer ec.mu.Unlock()
	return ec.closeCode
}

// blackholeProxy forwards TCP to target until blackhole() silently drops all current connections
// (a half-open path, like a laptop that slept or changed networks). New connections still work.
type blackholeProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	dead   map[net.Conn]bool
	conns  []net.Conn
}

func newBlackholeProxy(t *testing.T, target string) *blackholeProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &blackholeProxy{ln: ln, target: target, dead: map[net.Conn]bool{}}
	t.Cleanup(func() { _ = ln.Close() })
	go p.serve()
	return p
}

func (p *blackholeProxy) addr() string { return p.ln.Addr().String() }

func (p *blackholeProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		go p.pipe(c, up, c)
		go p.pipe(up, c, c)
	}
}

// pipe copies src→dst unless the client conn `key` has been blackholed (then bytes vanish).
func (p *blackholeProxy) pipe(dst, src, key net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if err != nil {
			return
		}
		p.mu.Lock()
		dead := p.dead[key]
		p.mu.Unlock()
		if dead {
			continue
		}
		if _, err := dst.Write(buf[:n]); err != nil {
			return
		}
	}
}

func (p *blackholeProxy) blackhole() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		p.dead[c] = true
	}
}
