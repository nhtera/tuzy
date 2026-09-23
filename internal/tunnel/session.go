package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// AccessEntry describes one relayed request (for the access log / inspector).
type AccessEntry struct {
	StreamID uint32
	Kind     string // "http" | "ws"
	Method   string
	Path     string
	Status   int
	Bytes    int64
	Duration time.Duration
	RemoteIP string
	Err      string
}

// sessionConfig is shared by every stream of a session.
type sessionConfig struct {
	target        *url.URL // e.g. http://localhost:3000
	hostHeader    string   // "preserve" (default) | "rewrite"
	transport     http.RoundTripper
	pingInterval  time.Duration
	pongTimeout   time.Duration
	probeTimeout  time.Duration // out-of-cycle ping deadline after sleep / network change
	probeEvery    time.Duration // sleep-skew + interface-change check period
	creditTimeout time.Duration
	drainGrace    time.Duration
	onAccess      func(AccessEntry)
	logger        *slog.Logger
}

var (
	errHeartbeat   = errors.New("heartbeat timeout: nothing received from edge")
	errProtocol    = errors.New("protocol violation")
	errSessionDone = errors.New("session closed")
)

type outFrame struct {
	typ  websocket.MessageType
	data []byte
	// closeCode, when set, makes the writer close the socket after everything queued before it
	// (graceful DRAIN: in-flight responses are fully written first).
	closeCode websocket.StatusCode
}

// streamHandler is one live stream. onFrame must never block (the reader calls it).
type streamHandler interface {
	onFrame(f protocol.Frame) error
	abort()
}

// session is one agent WebSocket connection (one epoch at the edge).
type session struct {
	cfg        *sessionConfig
	conn       *websocket.Conn
	ctx        context.Context
	cancel     context.CancelCauseFunc
	ctrl       chan outFrame // WINDOW, ACK, RESET, DRAIN, pings: always written first
	data       chan outFrame // heads, bodies, ends, WS messages: ordered per stream
	connCredit *credit
	alive      chan struct{} // signalled on every inbound message (pong or frame)
	probe      chan struct{} // request an out-of-cycle liveness check
	wg         sync.WaitGroup

	mu            sync.Mutex
	streams       map[uint32]streamHandler
	maxSeen       uint32
	connUngranted int64
	goaway        *protocol.GoawayMsg
}

func newSession(conn *websocket.Conn, cfg *sessionConfig) *session {
	ctx, cancel := context.WithCancelCause(context.Background())
	conn.SetReadLimit(protocol.HeaderSize + protocol.MaxWSMessage + 1024)
	return &session{
		cfg:        cfg,
		conn:       conn,
		ctx:        ctx,
		cancel:     cancel,
		ctrl:       make(chan outFrame, 256),
		data:       make(chan outFrame, 16), // small: limits head-of-line blocking across streams
		connCredit: newCredit(protocol.ConnWindow),
		alive:      make(chan struct{}, 1),
		probe:      make(chan struct{}, 1),
		streams:    make(map[uint32]streamHandler),
	}
}

// handshake sends HELLO and waits for READY (or GOAWAY) before the loops start.
func (s *session) handshake(ctx context.Context, hello protocol.HelloMsg, timeout time.Duration) (*protocol.ReadyMsg, *protocol.GoawayMsg, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, err := protocol.EncodeJSON(protocol.Hello, 0, hello)
	if err != nil {
		return nil, nil, err
	}
	if err := s.conn.Write(ctx, websocket.MessageBinary, b); err != nil {
		return nil, nil, err
	}
	for {
		typ, msg, err := s.conn.Read(ctx)
		if err != nil {
			return nil, nil, err
		}
		if typ != websocket.MessageBinary {
			continue
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			return nil, nil, err
		}
		switch f.Type {
		case protocol.Ready:
			var r protocol.ReadyMsg
			if err := f.UnmarshalPayload(&r); err != nil {
				return nil, nil, err
			}
			if r.Name == "" || r.URL == "" {
				return nil, nil, fmt.Errorf("%w: READY without name/url", errProtocol)
			}
			return &r, nil, nil
		case protocol.Goaway:
			var g protocol.GoawayMsg
			if err := f.UnmarshalPayload(&g); err != nil {
				return nil, nil, err
			}
			if g.Reason == "" {
				return nil, nil, fmt.Errorf("%w: GOAWAY without reason", errProtocol)
			}
			return nil, &g, nil
		default:
			return nil, nil, fmt.Errorf("%w: %s before READY", errProtocol, f.Type)
		}
	}
}

// run relays streams until the connection ends. Cancelling userCtx drains gracefully (§6 DRAIN).
// It returns the cause (a websocket close error, errHeartbeat, errProtocol, or nil on drain).
func (s *session) run(userCtx context.Context) error {
	go s.writeLoop()
	go s.heartbeatLoop()
	go s.probeLoop()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		s.readLoop()
	}()

	select {
	case <-s.ctx.Done():
	case <-userCtx.Done():
		s.drain()
	}
	<-readDone
	s.mu.Lock()
	streams := s.streams
	s.streams = map[uint32]streamHandler{}
	s.mu.Unlock()
	for _, h := range streams {
		h.abort()
	}
	s.wg.Wait()
	cause := context.Cause(s.ctx)
	if errors.Is(cause, errSessionDone) {
		return nil
	}
	return cause
}

// drain sends DRAIN, lets in-flight streams finish (bounded), then closes normally.
func (s *session) drain() {
	s.sendCtrl(protocol.Encode(protocol.Drain, 0, nil))
	done := make(chan struct{})
	go func() {
		for {
			s.mu.Lock()
			n := len(s.streams)
			s.mu.Unlock()
			if n == 0 {
				close(done)
				return
			}
			select {
			case <-time.After(50 * time.Millisecond):
			case <-s.ctx.Done():
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(s.cfg.drainGrace):
	}
	// Queue the close behind every frame the streams already queued, so responses finish intact.
	select {
	case s.data <- outFrame{closeCode: websocket.StatusNormalClosure}:
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(s.cfg.drainGrace):
		}
	case <-s.ctx.Done():
		return
	case <-time.After(time.Second):
	}
	s.closeWith(websocket.StatusNormalClosure, "agent shutting down", errSessionDone)
}

// fail ends the session with cause (first cause wins) and drops the connection.
func (s *session) fail(cause error) {
	s.cancel(cause)
	_ = s.conn.CloseNow()
}

// closeWith performs a close handshake (bounded) and ends the session.
func (s *session) closeWith(code websocket.StatusCode, reason string, cause error) {
	s.cancel(cause)
	go func() {
		_ = s.conn.Close(code, reason)
	}()
	time.AfterFunc(2*time.Second, func() { _ = s.conn.CloseNow() })
}

func (s *session) writeLoop() {
	write := func(f outFrame) bool {
		if f.closeCode != 0 {
			s.closeWith(f.closeCode, "agent shutting down", errSessionDone)
			return false
		}
		// Not derived from s.ctx: a frame being written when the session winds down must not be
		// cut mid-message. fail() → CloseNow unblocks a write on a dead socket.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.conn.Write(ctx, f.typ, f.data); err != nil {
			s.fail(fmt.Errorf("write: %w", err))
			return false
		}
		return true
	}
	for {
		select {
		case f := <-s.ctrl:
			if !write(f) {
				return
			}
			continue
		default:
		}
		select {
		case f := <-s.ctrl:
			if !write(f) {
				return
			}
		case f := <-s.data:
			if !write(f) {
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// heartbeatLoop pings every pingInterval (and on demand after sleep / network change). The session
// is dead only when NOTHING arrives before the deadline: any inbound frame proves the path works,
// so a pong stuck behind a large upload in the send buffer is not a false alarm.
func (s *session) heartbeatLoop() {
	ticker := time.NewTicker(s.cfg.pingInterval)
	defer ticker.Stop()
	deadline := s.cfg.pongTimeout
	for {
		select {
		case <-s.alive: // drop a stale signal
		default:
		}
		select {
		case s.ctrl <- outFrame{typ: websocket.MessageText, data: []byte(protocol.Ping)}:
		case <-s.ctx.Done():
			return
		}
		if !s.awaitAlive(deadline) {
			return
		}
		select {
		case <-ticker.C:
			deadline = s.cfg.pongTimeout
		case <-s.probe:
			deadline = s.cfg.probeTimeout
		case <-s.ctx.Done():
			return
		}
	}
}

// awaitAlive waits for any inbound message. A probe request while waiting shortens the deadline
// (and re-pings). Returns false when the session is over (failed or closed).
func (s *session) awaitAlive(deadline time.Duration) bool {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	end := time.Now().Add(deadline)
	for {
		select {
		case <-s.alive:
			return true
		case <-timer.C:
			s.fail(errHeartbeat)
			return false
		case <-s.probe:
			if time.Until(end) > s.cfg.probeTimeout {
				end = time.Now().Add(s.cfg.probeTimeout)
				timer.Reset(s.cfg.probeTimeout)
				select {
				case s.ctrl <- outFrame{typ: websocket.MessageText, data: []byte(protocol.Ping)}:
				default:
				}
			}
		case <-s.ctx.Done():
			return false
		}
	}
}

// probeLoop requests an immediate liveness check when the machine resumed from sleep (wall clock
// jumped past the monotonic clock) or the network interfaces changed, so a half-open socket is
// detected in seconds instead of waiting up to pingInterval + pongTimeout.
func (s *session) probeLoop() {
	ticker := time.NewTicker(s.cfg.probeEvery)
	defer ticker.Stop()
	last := time.Now()
	addrs := interfaceFingerprint()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-ticker.C:
			slept := now.Round(0).Sub(last.Round(0))-now.Sub(last) > 3*time.Second
			last = now
			cur := interfaceFingerprint()
			changed := cur != addrs
			addrs = cur
			if slept || changed {
				select {
				case s.probe <- struct{}{}:
				default:
				}
			}
		}
	}
}

// interfaceFingerprint summarizes the host's interface addresses (no cgo).
func interfaceFingerprint() string {
	as, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(as))
	for _, a := range as {
		parts = append(parts, a.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (s *session) readLoop() {
	for {
		typ, msg, err := s.conn.Read(context.Background())
		if err != nil {
			s.fail(err)
			return
		}
		select {
		case s.alive <- struct{}{}:
		default:
		}
		if typ == websocket.MessageText {
			if string(msg) == protocol.Pong {
				continue
			}
			s.closeWith(websocket.StatusUnsupportedData, "unexpected text message", fmt.Errorf("%w: unexpected text message", errProtocol))
			return
		}
		f, err := protocol.Decode(msg)
		if err != nil {
			var pe *protocol.Error
			if errors.As(err, &pe) && pe.Kind == protocol.KindStreamError {
				s.streamError(pe)
				continue
			}
			s.closeWith(websocket.StatusProtocolError, truncateReason(err.Error()), fmt.Errorf("%w: %v", errProtocol, err))
			return
		}
		if err := s.dispatch(f); err != nil {
			s.closeWith(websocket.StatusProtocolError, truncateReason(err.Error()), fmt.Errorf("%w: %v", errProtocol, err))
			return
		}
	}
}

// dispatch routes one frame. It never blocks on a local consumer.
func (s *session) dispatch(f protocol.Frame) error {
	switch f.Type {
	case protocol.ReqHead:
		return s.openStream(f)
	case protocol.Window:
		if f.StreamID == 0 {
			return s.connCredit.add(int64(f.Value))
		}
	case protocol.Goaway:
		var g protocol.GoawayMsg
		if err := f.UnmarshalPayload(&g); err != nil {
			return err
		}
		s.mu.Lock()
		s.goaway = &g
		s.mu.Unlock()
		return nil
	case protocol.ReqBody, protocol.ReqEnd, protocol.Reset, protocol.WSText, protocol.WSBinary, protocol.WSClose, protocol.Ack:
	default:
		return fmt.Errorf("unexpected %s from edge", f.Type)
	}
	h := s.stream(f.StreamID)
	if h == nil {
		if f.Type == protocol.ReqBody {
			s.grantConn(len(f.Payload)) // discarded data still returns connection credit (§4.3)
		}
		return nil
	}
	return h.onFrame(f)
}

func (s *session) openStream(f protocol.Frame) error {
	s.mu.Lock()
	if f.StreamID <= s.maxSeen {
		s.mu.Unlock()
		return fmt.Errorf("REQ_HEAD id %d not above high-water mark %d", f.StreamID, s.maxSeen)
	}
	s.maxSeen = f.StreamID
	s.mu.Unlock()

	var head protocol.ReqHeadMsg
	if err := f.UnmarshalPayload(&head); err != nil || head.Method == "" || (head.Kind != "http" && head.Kind != "ws") {
		s.sendReset(f.StreamID, protocol.ResetProtocolError, "bad REQ_HEAD")
		return nil
	}
	ctx, cancel := context.WithCancel(s.ctx)
	var h streamHandler
	if head.Kind == "ws" {
		h = newWSStream(ctx, cancel, s, f.StreamID, head)
	} else {
		h = newHTTPStream(ctx, cancel, s, f.StreamID, head)
	}
	s.mu.Lock()
	s.streams[f.StreamID] = h
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.removeStream(f.StreamID)
		switch st := h.(type) {
		case *httpStream:
			st.run()
		case *wsStream:
			st.run()
		}
	}()
	return nil
}

func (s *session) streamError(pe *protocol.Error) {
	if h, ok := s.stream(pe.StreamID).(*wsStream); ok {
		h.localClose(pe.CloseCode, pe.Msg)
	}
}

func (s *session) stream(id uint32) streamHandler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[id]
}

func (s *session) removeStream(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	idle := len(s.streams) == 0
	s.mu.Unlock()
	if idle {
		s.flushConn()
	}
}

func (s *session) goawayMsg() *protocol.GoawayMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goaway
}

// sendCtrl queues a priority frame; it gives up only when the session is over. The reader may
// block here briefly if 256 control frames are queued (only when the socket itself is stalled,
// which the heartbeat then detects).
func (s *session) sendCtrl(b []byte) {
	select {
	case s.ctrl <- outFrame{typ: websocket.MessageBinary, data: b}:
	case <-s.ctx.Done():
	}
}

// sendData queues an ordered frame, blocking under writer backpressure.
func (s *session) sendData(ctx context.Context, b []byte) error {
	select {
	case s.data <- outFrame{typ: websocket.MessageBinary, data: b}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return errSessionDone
	}
}

func (s *session) sendJSON(ctx context.Context, t protocol.FrameType, id uint32, v any) error {
	b, err := protocol.EncodeJSON(t, id, v)
	if err != nil {
		return err
	}
	return s.sendData(ctx, b)
}

// sendBody streams r as RES_BODY chunks under stream + connection credit (§4.1).
func (s *session) sendBody(ctx context.Context, id uint32, stream *credit, r io.Reader) (int64, error) {
	buf := make([]byte, protocol.MaxBodyChunk)
	var total int64
	for {
		n, rerr := r.Read(buf)
		for off := 0; off < n; {
			k, err := acquire(ctx, stream, s.connCredit, n-off, s.cfg.creditTimeout)
			if err != nil {
				return total, err
			}
			if err := s.sendData(ctx, protocol.Encode(protocol.ResBody, id, buf[off:off+k])); err != nil {
				return total, err
			}
			off += k
			total += int64(k)
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// sendSmallResponse sends a complete agent-generated response (errors, refused upgrades), with
// its body under credit like any other body.
func (s *session) sendSmallResponse(ctx context.Context, id uint32, stream *credit, status int, headers []protocol.Header, body string) {
	if err := s.sendJSON(ctx, protocol.ResHead, id, protocol.ResHeadMsg{Status: status, Headers: headers}); err != nil {
		return
	}
	if _, err := s.sendBody(ctx, id, stream, strings.NewReader(body)); err != nil {
		s.sendReset(id, protocol.ResetLocalError, "could not send response")
		return
	}
	_ = s.sendData(ctx, protocol.Encode(protocol.ResEnd, id, nil))
}

func (s *session) sendReset(id uint32, code, msg string) {
	b, _ := protocol.EncodeJSON(protocol.Reset, id, protocol.ResetMsg{Code: code, Message: msg})
	s.sendCtrl(b)
}

// grantConn returns connection receive credit for n consumed/discarded bytes, coalesced at ¼ window
// while streams are active and flushed at once when idle (credit must never sit unreturned).
func (s *session) grantConn(n int) {
	s.mu.Lock()
	s.connUngranted += int64(n)
	grant := int64(0)
	if s.connUngranted >= protocol.ConnWindow/4 || len(s.streams) == 0 {
		grant = s.connUngranted
		s.connUngranted = 0
	}
	s.mu.Unlock()
	if grant > 0 {
		s.sendCtrl(protocol.EncodeU32(protocol.Window, 0, uint32(grant)))
	}
}

// flushConn sends any coalesced connection credit now (on stream end).
func (s *session) flushConn() {
	s.mu.Lock()
	grant := s.connUngranted
	s.connUngranted = 0
	s.mu.Unlock()
	if grant > 0 {
		s.sendCtrl(protocol.EncodeU32(protocol.Window, 0, uint32(grant)))
	}
}
