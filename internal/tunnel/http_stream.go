package tunnel

import (
	"bytes"
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
	rec        StreamRecorder

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
		h.rec.RequestEnd() // the visitor's request body ended normally on the wire (inspector hook)
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
		h.rec.RequestBody(chunk) // inspector tee (capped, non-blocking)
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
		h.rec.End(entry.Err)
		if h.s.cfg.onAccess != nil {
			h.s.cfg.onAccess(entry)
		}
	}()

	pr, pw := io.Pipe()
	go h.feed(pw)
	defer func() { _ = pr.CloseWithError(errStreamDone) }()

	preserved := h.s.cfg.hostMode() == "preserve" // Host policy this request is sent with
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
	// A dev server that refuses the public Host (Vite allowedHosts, webpack "Invalid Host header",
	// Rails/Django host checks) answers 400/403 before reading the body. Peek at such an answer:
	// in auto mode switch the tunnel to rewriting the Host and replay bodyless requests at once,
	// so the visitor never sees the error.
	var peeked []byte
	if preserved && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest) {
		peeked, _ = io.ReadAll(io.LimitReader(resp.Body, hostCheckSniffLimit))
		if isHostCheckRejection(peeked) {
			if h.s.cfg.hostHeader == "auto" {
				h.s.cfg.learnHostRewrite()
				if req.Body == http.NoBody {
					if retry, rerr := h.buildRequest(http.NoBody); rerr == nil {
						if r2, rerr := h.s.cfg.transport.RoundTrip(retry); rerr == nil {
							_ = resp.Body.Close()
							resp, peeked = r2, nil
						}
					}
				}
			} else {
				entry.HostRejected = true // explicit preserve: the CLI explains the options
			}
		}
	}
	defer func() { _ = resp.Body.Close() }()
	entry.Status = resp.StatusCode
	go h.watchEarlyResponse(pr)

	headers := responseHeaders(resp)
	h.rec.Response(resp.StatusCode, headers)
	if err := h.s.sendJSON(h.ctx, protocol.ResHead, h.id, protocol.ResHeadMsg{Status: resp.StatusCode, Headers: headers}); err != nil {
		return
	}
	body := io.Reader(resp.Body)
	if peeked != nil { // relay the peeked prefix, then the rest
		body = io.MultiReader(bytes.NewReader(peeked), resp.Body)
	}
	n, err := h.s.sendBody(h.ctx, h.id, h.sendCredit, io.TeeReader(body, recorderWriter{h.rec}))
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
	headers := []protocol.Header{{"content-type", "text/plain; charset=utf-8"}}
	h.rec.Response(http.StatusBadGateway, headers)
	h.rec.ResponseBody([]byte(body))
	h.s.sendSmallResponse(h.ctx, h.id, h.sendCredit, http.StatusBadGateway, headers, body)
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

// buildRequest maps REQ_HEAD onto a request to the local target. The body is streamed (its total
// length is not known upfront: bodyLen -1 tells BuildLocalRequest to infer it from the visitor's
// headers, same as the live path always has).
func (h *httpStream) buildRequest(body io.Reader) (*http.Request, error) {
	return BuildLocalRequest(h.ctx, h.s.cfg.target, h.s.cfg.hostMode(), h.head.Method, h.head.Path, h.head.Headers, body, -1)
}

// BuildLocalRequest builds the *http.Request sent to a local target, shared by the live relay path
// (http_stream.go, above) and the inspector's replay so the hop-by-hop header list, the
// Content-Length/NoBody-by-method handling and the Fragment reset stay byte-identical between them.
//
// bodyLen is the exact number of body bytes when the caller already knows it (replay: a fully
// buffered body — this always wins over any Content-Length header, which may be stale after an
// edit). Pass -1 when body is streamed and of unknown length: ContentLength is then inferred from
// the Content-Length / Transfer-Encoding headers, falling back to a per-method heuristic (the live
// path, where the body has not been read yet).
func BuildLocalRequest(ctx context.Context, target *url.URL, hostHeaderMode, method, path string, headers []protocol.Header, body io.Reader, bodyLen int64) (*http.Request, error) {
	u, err := targetURL(target, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}

	var visitorHost string
	headerContentLength := int64(-1)
	chunked := false
	for _, kv := range headers {
		name := strings.ToLower(kv[0])
		switch {
		case name == "host":
			visitorHost = kv[1]
			continue
		case name == "content-length":
			if n, err := strconv.ParseInt(kv[1], 10, 64); err == nil && n >= 0 {
				headerContentLength = n
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
	if hostHeaderMode != "rewrite" && visitorHost != "" {
		req.Host = visitorHost
	}
	switch {
	case bodyLen >= 0:
		req.ContentLength = bodyLen // caller already knows the exact length (e.g. replay)
		if bodyLen == 0 {
			req.Body = http.NoBody
		}
	case headerContentLength >= 0:
		req.ContentLength = headerContentLength
		if headerContentLength == 0 {
			req.Body = http.NoBody
		}
	case chunked || methodMayHaveBody(method):
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
