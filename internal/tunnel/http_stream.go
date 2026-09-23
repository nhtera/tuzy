package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nhtera/tuzy/internal/protocol"
)

// hopByHop headers are never forwarded in either direction (RFC 9110 §7.6.1).
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"proxy-connection": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

var (
	errStreamDone    = errors.New("stream done")
	errEarlyResponse = errors.New("local app responded without reading the request body")
)

// earlyResponseGrace: once the local app has responded, a request body it hasn't consumed for this
// long is discarded (credit still returned), so the edge never stalls on credit while a streaming
// response is live. Apps that keep reading (full duplex) are unaffected.
const earlyResponseGrace = 2 * time.Second

// httpStream relays one visitor HTTP request to the local target (PROTOCOL.md §3.1, §4).
type httpStream struct {
	s          *session
	id         uint32
	head       protocol.ReqHeadMsg
	ctx        context.Context
	cancel     context.CancelFunc
	sendCredit *credit

	mu       sync.Mutex
	queue    [][]byte // request body chunks waiting for the local app (bounded by StreamWindow)
	queued   int
	reqEnded bool
	fed      bool // the feeder has exited: late body is granted straight back
	wake     chan struct{}
	progress atomic.Int64 // unix nanos of the last chunk the local app consumed
}

func newHTTPStream(ctx context.Context, cancel context.CancelFunc, s *session, id uint32, head protocol.ReqHeadMsg) *httpStream {
	return &httpStream{
		s: s, id: id, head: head, ctx: ctx, cancel: cancel,
		sendCredit: newCredit(protocol.StreamWindow),
		wake:       make(chan struct{}, 1),
	}
}

// onFrame is called by the session reader; it only queues and signals.
func (h *httpStream) onFrame(f protocol.Frame) error {
	switch f.Type {
	case protocol.ReqBody:
		h.mu.Lock()
		if h.fed {
			h.mu.Unlock()
			h.s.grantConn(len(f.Payload)) // late data for a finished stream (§4.3)
			return nil
		}
		if h.queued+len(f.Payload) > protocol.StreamWindow {
			h.mu.Unlock()
			h.s.grantConn(len(f.Payload))
			h.s.sendReset(h.id, protocol.ResetFlowControl, "request body exceeded stream window")
			h.cancel()
			return nil
		}
		h.queue = append(h.queue, f.Payload)
		h.queued += len(f.Payload)
		h.mu.Unlock()
		h.signal()
	case protocol.ReqEnd:
		h.mu.Lock()
		h.reqEnded = true
		h.mu.Unlock()
		h.signal()
	case protocol.Window:
		return h.sendCredit.add(int64(f.Value))
	case protocol.Reset:
		h.cancel()
	}
	return nil
}

func (h *httpStream) abort() { h.cancel() }

func (h *httpStream) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// feed moves queued body chunks into the pipe the local request reads from, granting WINDOW as the
// local app consumes. Once the app stops reading (pipe closed), data is discarded but credit is
// still returned, so an early response never deadlocks the edge (§4.3).
func (h *httpStream) feed(pw *io.PipeWriter) {
	discard := false
	ungranted := 0
	defer func() {
		// Return credit for anything still queued when the stream ends; later data is granted
		// directly by onFrame.
		h.mu.Lock()
		left := h.queued
		h.queue, h.queued = nil, 0
		h.fed = true
		h.mu.Unlock()
		if left > 0 {
			h.s.grantConn(left)
		}
		h.s.flushConn()
	}()
	for {
		h.mu.Lock()
		var chunk []byte
		if len(h.queue) > 0 {
			chunk = h.queue[0]
			h.queue = h.queue[1:]
		}
		ended := h.reqEnded && chunk == nil
		h.mu.Unlock()

		if ended {
			_ = pw.Close()
			return
		}
		if chunk == nil {
			select {
			case <-h.wake:
				continue
			case <-h.ctx.Done():
				_ = pw.CloseWithError(h.ctx.Err())
				return
			}
		}
		if !discard {
			if _, err := pw.Write(chunk); err != nil {
				discard = true
			}
		}
		h.progress.Store(time.Now().UnixNano())
		h.mu.Lock()
		h.queued -= len(chunk)
		h.mu.Unlock()
		h.s.grantConn(len(chunk))
		ungranted += len(chunk)
		if ungranted >= protocol.StreamWindow/4 {
			h.s.sendCtrl(protocol.EncodeU32(protocol.Window, h.id, uint32(ungranted)))
			ungranted = 0
		}
	}
}

func (h *httpStream) run() {
	defer h.cancel()
	start := time.Now()
	entry := AccessEntry{StreamID: h.id, Kind: "http", Method: h.head.Method, Path: h.head.Path, RemoteIP: h.head.RemoteIP}
	defer func() {
		entry.Duration = time.Since(start)
		if h.s.cfg.onAccess != nil {
			h.s.cfg.onAccess(entry)
		}
	}()

	pr, pw := io.Pipe()
	go h.feed(pw)
	defer func() { _ = pr.CloseWithError(errStreamDone) }()

	req, err := h.buildRequest(pr)
	if err != nil {
		entry.Status, entry.Err = 502, err.Error()
		h.sendLocalError(fmt.Sprintf("tuzy: bad request for local target: %v\n", err))
		return
	}
	if req.Body == http.NoBody {
		_ = pr.CloseWithError(errStreamDone) // no body expected: discard (and credit) anything that comes
	}
	resp, err := h.s.cfg.transport.RoundTrip(req)
	if err != nil {
		if h.ctx.Err() != nil {
			entry.Err = "cancelled"
			return
		}
		entry.Status, entry.Err = 502, err.Error()
		h.sendLocalError(fmt.Sprintf("tuzy: could not reach %s\n", h.s.cfg.target))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	entry.Status = resp.StatusCode
	go h.watchEarlyResponse(pr)

	if err := h.s.sendJSON(h.ctx, protocol.ResHead, h.id, protocol.ResHeadMsg{Status: resp.StatusCode, Headers: responseHeaders(resp)}); err != nil {
		return
	}
	n, err := h.s.sendBody(h.ctx, h.id, h.sendCredit, resp.Body)
	entry.Bytes = n
	switch {
	case err == nil:
		_ = h.s.sendData(h.ctx, protocol.Encode(protocol.ResEnd, h.id, nil))
	case errors.Is(err, errCreditTimeout):
		entry.Err = "credit timeout"
		h.s.sendReset(h.id, protocol.ResetCreditTimeout, "no credit from edge")
	case h.ctx.Err() != nil:
		entry.Err = "cancelled"
	default:
		entry.Err = err.Error()
		h.s.sendReset(h.id, protocol.ResetLocalError, "local response failed")
	}
}

// watchEarlyResponse discards the request body if the local app stops consuming it after it has
// started responding (see earlyResponseGrace).
func (h *httpStream) watchEarlyResponse(pr *io.PipeReader) {
	h.progress.Store(time.Now().UnixNano())
	t := time.NewTicker(earlyResponseGrace / 4)
	defer t.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-t.C:
		}
		h.mu.Lock()
		pending, ended, fed := h.queued, h.reqEnded, h.fed
		h.mu.Unlock()
		if fed || (ended && pending == 0) {
			return
		}
		if pending > 0 && time.Since(time.Unix(0, h.progress.Load())) > earlyResponseGrace {
			_ = pr.CloseWithError(errEarlyResponse)
			return
		}
	}
}

func (h *httpStream) sendLocalError(body string) {
	h.s.sendSmallResponse(h.ctx, h.id, h.sendCredit, http.StatusBadGateway, []protocol.Header{{"content-type", "text/plain; charset=utf-8"}}, body)
}

// targetURL joins the local target with the visitor's request-URI, keeping its escaping exactly.
func targetURL(target *url.URL, requestURI string) (*url.URL, error) {
	ref, err := url.ParseRequestURI(requestURI)
	if err != nil {
		return nil, err
	}
	u := *target
	base := strings.TrimRight(target.Path, "/")
	u.Path = base + ref.Path
	u.RawPath = strings.TrimRight(target.EscapedPath(), "/") + ref.EscapedPath()
	u.RawQuery = ref.RawQuery
	u.Fragment = ""
	return &u, nil
}

// buildRequest maps REQ_HEAD onto a request to the local target.
func (h *httpStream) buildRequest(body io.Reader) (*http.Request, error) {
	u, err := targetURL(h.s.cfg.target, h.head.Path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(h.ctx, h.head.Method, u.String(), body)
	if err != nil {
		return nil, err
	}

	var visitorHost string
	contentLength := int64(-1)
	chunked := false
	for _, kv := range h.head.Headers {
		name := strings.ToLower(kv[0])
		switch {
		case name == "host":
			visitorHost = kv[1]
			continue
		case name == "content-length":
			if n, err := strconv.ParseInt(kv[1], 10, 64); err == nil && n >= 0 {
				contentLength = n
			}
			continue
		case name == "transfer-encoding":
			chunked = true
			continue
		case hopByHop[name]:
			continue
		}
		req.Header.Add(kv[0], kv[1])
	}
	if h.s.cfg.hostHeader != "rewrite" && visitorHost != "" {
		req.Host = visitorHost
	}
	switch {
	case contentLength >= 0:
		req.ContentLength = contentLength
		if contentLength == 0 {
			req.Body = http.NoBody
		}
	case chunked || methodMayHaveBody(h.head.Method):
		req.ContentLength = -1 // unknown length: streamed (chunked) to the local app
	default:
		req.Body = http.NoBody
		req.ContentLength = 0
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header["User-Agent"] = nil // don't add Go's default UA
	}
	return req, nil
}

func methodMayHaveBody(m string) bool {
	switch strings.ToUpper(m) {
	case "GET", "HEAD", "OPTIONS", "DELETE", "TRACE", "CONNECT":
		return false
	}
	return true
}

// responseHeaders converts the local response headers to RES_HEAD pairs, dropping hop-by-hop
// headers and any header the app listed in Connection (RFC 9110 §7.6.1).
func responseHeaders(resp *http.Response) []protocol.Header {
	listed := map[string]bool{}
	for _, v := range resp.Header.Values("Connection") {
		for _, t := range strings.Split(v, ",") {
			listed[strings.ToLower(strings.TrimSpace(t))] = true
		}
	}
	out := make([]protocol.Header, 0, len(resp.Header))
	for name, values := range resp.Header {
		lname := strings.ToLower(name)
		if hopByHop[lname] || listed[lname] {
			continue
		}
		for _, v := range values {
			out = append(out, protocol.Header{name, v})
		}
	}
	return out
}
