package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

type wsMsg struct {
	typ  websocket.MessageType
	data []byte
}

// wsStream relays one visitor WebSocket to a local WebSocket (PROTOCOL.md §5).
//
//	edge → local: queued by the session reader (≤ WSMsgTolerance), written by run(), ACKed after delivery.
//	local → edge: read by pumpLocal(), each message needs one of WSMsgCredit unacked slots.
type wsStream struct {
	s          *session
	id         uint32
	head       protocol.ReqHeadMsg
	ctx        context.Context
	cancel     context.CancelFunc
	credit     *msgCredit
	sendCredit *credit // byte credit for a non-101 answer (refused upgrade)
	rec        StreamRecorder

	mu          sync.Mutex
	unacked     int                // edge→agent messages received and not yet ACKed (§5.3 tolerance)
	abortWrite  context.CancelFunc // interrupts a local write stuck on an app that stopped reading
	inbox       []wsMsg
	remoteClose *protocol.WSCloseMsg // edge sent WS_CLOSE
	localReq    *protocol.WSCloseMsg // we must close the local socket (protocol/stream error)
	closeSent   bool                 // we sent WS_CLOSE to the edge
	wake        chan struct{}
}

func newWSStream(ctx context.Context, cancel context.CancelFunc, s *session, id uint32, head protocol.ReqHeadMsg) *wsStream {
	return &wsStream{
		s: s, id: id, head: head, ctx: ctx, cancel: cancel,
		credit: newMsgCredit(), sendCredit: newCredit(protocol.StreamWindow), wake: make(chan struct{}, 1),
	}
}

func (w *wsStream) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *wsStream) abort() { w.cancel() }

// interruptWrite unblocks a pending local write (a close was requested meanwhile).
func (w *wsStream) interruptWrite() {
	w.mu.Lock()
	f := w.abortWrite
	w.mu.Unlock()
	if f != nil {
		f()
	}
}

// onFrame is called by the session reader and never blocks.
func (w *wsStream) onFrame(f protocol.Frame) error {
	switch f.Type {
	case protocol.WSText, protocol.WSBinary:
		typ := websocket.MessageBinary
		if f.Type == protocol.WSText {
			typ = websocket.MessageText
		}
		w.mu.Lock()
		if w.unacked >= protocol.WSMsgTolerance {
			w.mu.Unlock()
			w.localClose(1008, "edge exceeded websocket message credit")
			return nil
		}
		w.unacked++
		w.inbox = append(w.inbox, wsMsg{typ: typ, data: f.Payload})
		w.mu.Unlock()
		w.signal()
	case protocol.WSClose:
		var c protocol.WSCloseMsg
		if err := f.UnmarshalPayload(&c); err != nil {
			w.s.sendReset(w.id, protocol.ResetProtocolError, "bad WS_CLOSE")
			w.cancel()
			return nil
		}
		w.mu.Lock()
		w.remoteClose = &c
		w.mu.Unlock()
		w.signal()
		w.interruptWrite()
	case protocol.Ack:
		w.credit.ack(int(f.Value))
	case protocol.Window:
		return w.sendCredit.add(int64(f.Value))
	case protocol.Reset:
		// Close the local socket with a proper close frame rather than dropping it.
		w.localClose(1001, "tunnel stream reset")
	case protocol.ReqBody:
		w.s.grantConn(len(f.Payload)) // never expected on a ws stream; still return credit
	}
	return nil
}

// localClose asks run() to close the local socket with code and tell the edge.
func (w *wsStream) localClose(code int, reason string) {
	w.mu.Lock()
	if w.localReq == nil {
		w.localReq = &protocol.WSCloseMsg{Code: code, Reason: reason}
	}
	w.mu.Unlock()
	w.signal()
	w.interruptWrite()
}

// markCloseSent reports whether this call is the first to send WS_CLOSE to the edge.
func (w *wsStream) markCloseSent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closeSent {
		return false
	}
	w.closeSent = true
	return true
}

func (w *wsStream) sendClose(code int, reason string) {
	if !w.markCloseSent() {
		return
	}
	_ = w.s.sendJSON(w.s.ctx, protocol.WSClose, w.id, protocol.WSCloseMsg{Code: sendableCode(code), Reason: truncateReason(reason)})
}

func (w *wsStream) run() {
	defer w.cancel()
	start := time.Now()
	entry := AccessEntry{StreamID: w.id, Kind: "ws", Method: w.head.Method, Path: w.head.Path, RemoteIP: w.head.RemoteIP}
	defer func() {
		entry.Duration = time.Since(start)
		w.rec.End(entry.Err)
		if w.s.cfg.onAccess != nil {
			w.s.cfg.onAccess(entry)
		}
	}()

	local, status, err := w.dial()
	entry.Status = status
	if err != nil {
		entry.Err = err.Error()
		return
	}
	defer func() { _ = local.CloseNow() }()

	var head []protocol.Header
	if sp := local.Subprotocol(); sp != "" {
		head = append(head, protocol.Header{"sec-websocket-protocol", sp})
	}
	w.rec.Response(http.StatusSwitchingProtocols, head)
	if err := w.s.sendJSON(w.ctx, protocol.ResHead, w.id, protocol.ResHeadMsg{Status: http.StatusSwitchingProtocols, Headers: head}); err != nil {
		return
	}

	localDone := make(chan struct{})
	go func() {
		defer close(localDone)
		w.pumpLocal(local)
	}()
	w.pumpEdge(local, localDone)
	w.cancel()
	<-localDone
}

// pumpEdge writes edge messages to the local socket (ACK after each delivery) and handles closes.
func (w *wsStream) pumpEdge(local *websocket.Conn, localDone <-chan struct{}) {
	for {
		w.mu.Lock()
		var msg *wsMsg
		if len(w.inbox) > 0 {
			m := w.inbox[0]
			w.inbox = w.inbox[1:]
			msg = &m
		}
		rc, lr := w.remoteClose, w.localReq
		w.mu.Unlock()

		switch {
		case msg != nil:
			if err := w.writeLocal(local, msg); err != nil {
				if w.ctx.Err() != nil {
					return
				}
				// Timed out (app stopped reading) unless a close interrupted it; either way the
				// remaining queue is dropped and the close is handled on the next passes.
				w.localClose(1011, "local app stopped reading")
				continue
			}
			w.mu.Lock()
			w.unacked--
			w.mu.Unlock()
			w.rec.WSMessage(true)
			w.s.sendCtrl(protocol.EncodeU32(protocol.Ack, w.id, 1))
			continue
		case rc != nil:
			// Edge (visitor) closed: close the local socket, answer WS_CLOSE unless we started it.
			closeLocal(local, rc.Code, rc.Reason)
			w.sendClose(rc.Code, rc.Reason)
			return
		case lr != nil:
			w.sendClose(lr.Code, lr.Reason)
			closeLocal(local, lr.Code, lr.Reason)
			return
		}
		select {
		case <-w.wake:
		case <-localDone:
			return
		case <-w.ctx.Done():
			return
		}
	}
}

// localWriteTimeout bounds one write to a local app that stopped reading its socket.
const localWriteTimeout = 30 * time.Second

// writeLocal writes one message; a requested close (interruptWrite) or the timeout aborts it.
func (w *wsStream) writeLocal(local *websocket.Conn, msg *wsMsg) error {
	ctx, cancel := context.WithTimeout(w.ctx, localWriteTimeout)
	w.mu.Lock()
	w.abortWrite = cancel
	pending := w.remoteClose != nil || w.localReq != nil
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.abortWrite = nil
		w.mu.Unlock()
		cancel()
	}()
	if pending {
		return context.Canceled
	}
	return local.Write(ctx, msg.typ, msg.data)
}

// pumpLocal reads local messages and relays them under message credit.
func (w *wsStream) pumpLocal(local *websocket.Conn) {
	local.SetReadLimit(protocol.MaxWSMessage)
	for {
		typ, data, err := local.Read(w.ctx)
		if err != nil {
			if w.ctx.Err() != nil {
				return
			}
			w.mu.Lock()
			pending := w.localReq != nil || w.remoteClose != nil
			w.mu.Unlock()
			if !pending { // a requested close (which may have torn the socket down) reports its own code
				code, reason := localCloseStatus(err)
				w.sendClose(code, reason)
			}
			w.signal()
			return
		}
		if err := w.credit.take(w.ctx); err != nil {
			return
		}
		ft := protocol.WSBinary
		if typ == websocket.MessageText {
			ft = protocol.WSText
		}
		if err := w.s.sendData(w.ctx, protocol.Encode(ft, w.id, data)); err != nil {
			return
		}
		w.rec.WSMessage(false)
	}
}

// dial connects to the local WebSocket. On failure it relays the local HTTP answer (or a 502) as a
// normal response and returns an error. It returns the status reported to the visitor.
func (w *wsStream) dial() (*websocket.Conn, int, error) {
	u, err := targetURL(w.s.cfg.target, w.head.Path)
	if err != nil {
		w.respond(http.StatusBadGateway, nil, "tuzy: bad websocket path\n")
		return nil, http.StatusBadGateway, err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	opts := &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: w.s.cfg.transport},
		HTTPHeader: http.Header{},
	}
	for _, kv := range w.head.Headers {
		name := strings.ToLower(kv[0])
		switch {
		case name == "host":
			if w.s.cfg.hostHeader != "rewrite" {
				opts.Host = kv[1]
			}
		case name == "sec-websocket-protocol":
			for _, p := range strings.Split(kv[1], ",") {
				if p = strings.TrimSpace(p); p != "" {
					opts.Subprotocols = append(opts.Subprotocols, p)
				}
			}
		case strings.HasPrefix(name, "sec-websocket-"), hopByHop[name]:
		default:
			opts.HTTPHeader.Add(kv[0], kv[1])
		}
	}
	dialCtx, cancel := context.WithTimeout(w.ctx, 30*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, u.String(), opts)
	if err == nil {
		return conn, http.StatusSwitchingProtocols, nil
	}
	if w.ctx.Err() != nil {
		return nil, 0, w.ctx.Err()
	}
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		// The local app refused the upgrade: relay its answer as a normal HTTP response.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		w.respond(resp.StatusCode, responseHeaders(resp), string(body))
		return nil, resp.StatusCode, fmt.Errorf("local websocket refused: %s", resp.Status)
	}
	w.respond(http.StatusBadGateway, []protocol.Header{{"content-type", "text/plain; charset=utf-8"}},
		fmt.Sprintf("tuzy: could not reach %s\n", w.s.cfg.target))
	return nil, http.StatusBadGateway, err
}

func (w *wsStream) respond(status int, headers []protocol.Header, body string) {
	w.rec.Response(status, headers)
	w.rec.ResponseBody([]byte(body))
	// Content-Length would be wrong after LimitReader truncation; let the edge stream it.
	filtered := headers[:0:0]
	for _, h := range headers {
		if !strings.EqualFold(h[0], "content-length") {
			filtered = append(filtered, h)
		}
	}
	w.s.sendSmallResponse(w.ctx, w.id, w.sendCredit, status, filtered, body)
}

// sendableCode maps codes that may not appear in a close frame to 1000.
func sendableCode(code int) int {
	if code < 1000 || code > 4999 || code == 1004 || code == 1005 || code == 1006 || code == 1015 {
		return 1000
	}
	return code
}

// truncateReason keeps a close reason within the 123-byte limit on a rune boundary.
func truncateReason(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	cut := 0
	for i := range s {
		if i > limit {
			break
		}
		cut = i
	}
	return s[:cut]
}

func closeLocal(c *websocket.Conn, code int, reason string) {
	done := make(chan struct{})
	go func() {
		_ = c.Close(websocket.StatusCode(sendableCode(code)), truncateReason(reason))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = c.CloseNow()
	}
}

// localCloseStatus derives the WS_CLOSE to send when the local socket ends.
func localCloseStatus(err error) (int, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code), ce.Reason
	}
	if errors.Is(err, websocket.ErrMessageTooBig) {
		return 1009, "message too big"
	}
	return 1001, "local app closed the connection"
}
