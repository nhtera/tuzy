package inspector

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nhtera/tuzy/internal/protocol"
	"github.com/nhtera/tuzy/internal/tunnel"
)

// Begin implements tunnel.Recorder: one entry per relayed request / visitor WebSocket.
func (s *Store) Begin(tunnelName string, head protocol.ReqHeadMsg) tunnel.StreamRecorder {
	e := &Entry{
		Tunnel: tunnelName, StartedAt: time.Now(), Kind: head.Kind,
		Method: head.Method, Path: head.Path, RemoteIP: head.RemoteIP,
		ReqHeaders: toPairs(head.Headers),
	}
	r := &streamRec{s: s, start: e.StartedAt, reqContentLength: declaredContentLength(head.Headers)}
	r.resContentLength.Store(-1)
	if head.Kind == "ws" {
		// Counted lock-free on the hot WebSocket path (H1/M2); folded into the entry on read/End.
		e.wsIn, e.wsOut = new(atomic.Int64), new(atomic.Int64)
		r.wsIn, r.wsOut = e.wsIn, e.wsOut
	}
	r.id = s.add(e)
	return r
}

func toPairs(hs []protocol.Header) [][2]string {
	out := make([][2]string, len(hs))
	for i, h := range hs {
		out[i] = [2]string(h)
	}
	return out
}

// declaredContentLength parses the visitor's Content-Length header, or -1 if absent/invalid.
func declaredContentLength(hs []protocol.Header) int64 {
	for _, h := range hs {
		if strings.EqualFold(h[0], "content-length") {
			if n, err := strconv.ParseInt(strings.TrimSpace(h[1]), 10, 64); err == nil && n >= 0 {
				return n
			}
		}
	}
	return -1
}

// streamRec tees one stream into the store, capped at MaxBodyCapture per side. The high-frequency
// hooks (RequestBody, ResponseBody, WSMessage) must never block the proxy path (M2): body hooks use
// Store.mutate (TryLock, drop-and-flag on contention) and WSMessage never touches the store at all.
type streamRec struct {
	s     *Store
	id    uint64
	start time.Time

	reqContentLength int64 // -1 unknown; the visitor's declared Content-Length, from REQ_HEAD
	resContentLength atomic.Int64
	reqSize, resSize atomic.Int64 // bytes seen so far, even ones dropped under store contention
	reqComplete      atomic.Bool  // RequestEnd() was called (REQ_END seen on the wire)
	reqDropped       atomic.Bool  // a RequestBody call was dropped under store contention (M2)
	resDropped       atomic.Bool
	wsIn, wsOut      *atomic.Int64 // shared with the Entry; nil for a "http" stream
}

// appendCapped adds p to dst up to MaxBodyCapture, reporting truncation. When dst is empty it
// preallocates from hint (a declared Content-Length) up to the cap, so a known-size body doesn't
// incur append's usual growth over-allocation (L1: cap() is what counts against MaxTotalBytes).
func appendCapped(dst []byte, p []byte, hint int64) ([]byte, bool) {
	if dst == nil && hint > 0 {
		want := hint
		if want > MaxBodyCapture {
			want = MaxBodyCapture
		}
		dst = make([]byte, 0, want)
	}
	room := MaxBodyCapture - len(dst)
	if room <= 0 {
		return dst, len(p) > 0
	}
	if len(p) > room {
		return append(dst, p[:room]...), true
	}
	return append(dst, p...), false
}

// fold syncs the atomics kept off the store lock (sizes, dropped-chunk flags) into e. Called at the
// start of every mutate/update closure so nothing is lost even when a chunk was dropped.
func (r *streamRec) fold(e *Entry) {
	e.ReqSize = r.reqSize.Load()
	e.ResSize = r.resSize.Load()
	if r.reqDropped.Swap(false) {
		e.ReqTrunc = true
	}
	if r.resDropped.Swap(false) {
		e.ResTrunc = true
	}
}

func (r *streamRec) RequestBody(p []byte) {
	r.reqSize.Add(int64(len(p)))
	if !r.s.mutate(r.id, func(e *Entry) {
		r.fold(e)
		if e.Evicted {
			return
		}
		var trunc bool
		e.ReqBody, trunc = appendCapped(e.ReqBody, p, r.reqContentLength)
		e.ReqTrunc = e.ReqTrunc || trunc
	}) {
		r.reqDropped.Store(true) // store busy: this chunk is lost (M2); folded in on the next update
	}
}

// RequestEnd implements tunnel.StreamRecorder: the visitor's request body ended normally on the
// wire (REQ_END). Its absence at End() means the body is incomplete (M1).
func (r *streamRec) RequestEnd() {
	r.reqComplete.Store(true)
}

func (r *streamRec) Response(status int, headers []protocol.Header) {
	r.resContentLength.Store(declaredContentLength(headers))
	r.s.update(r.id, func(e *Entry) {
		r.fold(e)
		e.Status = status
		e.ResHeaders = toPairs(headers)
	})
}

func (r *streamRec) ResponseBody(p []byte) {
	r.resSize.Add(int64(len(p)))
	if !r.s.mutate(r.id, func(e *Entry) {
		r.fold(e)
		if e.Evicted {
			return
		}
		var trunc bool
		e.ResBody, trunc = appendCapped(e.ResBody, p, r.resContentLength.Load())
		e.ResTrunc = e.ResTrunc || trunc
	}) {
		r.resDropped.Store(true)
	}
}

// WSMessage never touches the store: counted lock-free on the hot WebSocket path (H1 — publishing
// or locking per message would flood subscribers / contend the proxy). Folded into the entry by
// Entry.foldLive on every read (List/Get) and at End.
func (r *streamRec) WSMessage(fromVisitor bool) {
	if r.wsIn == nil {
		return // defensive: never expected on a "http" stream
	}
	if fromVisitor {
		r.wsIn.Add(1)
	} else {
		r.wsOut.Add(1)
	}
}

func (r *streamRec) End(errMsg string) {
	r.s.update(r.id, func(e *Entry) {
		r.fold(e)
		e.Done = true
		e.DurationMs = time.Since(r.start).Milliseconds()
		if errMsg != "" {
			e.Error = errMsg // "cancelled" included (M1): the visitor/local app aborting is worth showing
		}
		if e.Kind == "http" {
			e.ReqIncomplete = !r.reqComplete.Load() || (r.reqContentLength >= 0 && e.ReqSize < r.reqContentLength)
		}
	})
}
