package inspector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nhtera/tuzy/internal/protocol"
	"github.com/nhtera/tuzy/internal/tunnel"
)

// ReplayRequest is an (optionally edited) request to send to a tunnel's local target.
type ReplayRequest struct {
	Tunnel  string      `json:"tunnel"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Headers [][2]string `json:"headers"`
	Body    []byte      `json:"body_b64"` // base64 in JSON
}

// toProtocolHeaders converts the JSON header-pair shape used by the API/UI into the shape
// tunnel.BuildLocalRequest and the live path share (protocol.Header is [2]string).
func toProtocolHeaders(hs [][2]string) []protocol.Header {
	out := make([]protocol.Header, len(hs))
	for i, h := range hs {
		out[i] = protocol.Header(h)
	}
	return out
}

// replay sends req straight to the tunnel's local target (not through the edge) with the same
// host-header mode, and records the result as a new entry with replay_of. It shares
// tunnel.BuildLocalRequest with the live proxy path (M6) so the hop-by-hop header list, the
// Content-Length/NoBody-by-method handling and the target-URL join (incl. Fragment reset) are
// identical — only bodyLen differs: replay always knows the exact body length upfront.
func (s *Store) replay(ctx context.Context, rt http.RoundTripper, req ReplayRequest, replayOf uint64) (uint64, error) {
	t, ok := s.Tunnel(req.Tunnel)
	if !ok || t.Target == nil {
		return 0, fmt.Errorf("unknown tunnel %q", req.Tunnel)
	}
	if req.Method == "" || !strings.HasPrefix(req.Path, "/") {
		return 0, errors.New("method and a path starting with / are required")
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	hr, err := tunnel.BuildLocalRequest(ctx, t.Target, t.HostHeader, req.Method, req.Path,
		toProtocolHeaders(req.Headers), bytes.NewReader(req.Body), int64(len(req.Body)))
	if err != nil {
		return 0, fmt.Errorf("invalid path: %w", err)
	}

	e := &Entry{
		Tunnel: req.Tunnel, StartedAt: time.Now(), Kind: "http", Method: req.Method, Path: req.Path,
		RemoteIP: "replay", ReqHeaders: req.Headers, ReplayOf: replayOf,
	}
	e.ReqBody, e.ReqTrunc = appendCapped(nil, req.Body, int64(len(req.Body)))
	rec := &streamRec{s: s, start: e.StartedAt, reqContentLength: int64(len(req.Body))}
	rec.resContentLength.Store(-1)
	rec.reqSize.Store(int64(len(req.Body)))
	rec.reqComplete.Store(true) // the whole body is already in hand: a replay is always "complete"
	rec.id = s.add(e)

	resp, err := rt.RoundTrip(hr)
	if err != nil {
		rec.End(err.Error())
		return rec.id, nil // recorded as a failed replay
	}
	defer func() { _ = resp.Body.Close() }()
	hs := make([]protocol.Header, 0, len(resp.Header))
	for k, vs := range resp.Header {
		for _, v := range vs {
			hs = append(hs, protocol.Header{k, v})
		}
	}
	rec.Response(resp.StatusCode, hs)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			rec.ResponseBody(buf[:n])
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				rec.End(rerr.Error())
				return rec.id, nil
			}
			break
		}
	}
	rec.End("")
	return rec.id, nil
}

// fromEntry turns a recorded entry back into a replay request.
func fromEntry(e *Entry) ReplayRequest {
	return ReplayRequest{Tunnel: e.Tunnel, Method: e.Method, Path: e.Path, Headers: e.ReqHeaders, Body: e.ReqBody}
}
