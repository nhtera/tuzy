package inspector

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed ui
var uiFS embed.FS

// csp: captured data is untrusted internet input; the UI never needs inline code or remote content.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Server is the local inspector UI + API.
type Server struct {
	store     *Store
	transport http.RoundTripper // same config as the tunnel's local transport
	srv       *http.Server
	ln        net.Listener
	host      string // the loopback IP actually bound (L2): "127.0.0.1" or "::1"
	port      string

	baseCancel context.CancelFunc // cancels every in-flight handler's context (M8)
}

// Listen binds addr (e.g. "127.0.0.1:4040"). When the default port is busy it tries 4041–4049.
// Only loopback addresses are accepted (checked both before and after binding: L2).
func Listen(addr string, store *Store, transport http.RoundTripper) (*Server, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid --inspect-addr %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("the inspector only listens on loopback addresses, not %q", host)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil && port == "4040" {
		for p := 4041; p <= 4049 && err != nil; p++ {
			ln, err = net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		}
	}
	if err != nil {
		return nil, err
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcpAddr.IP.IsLoopback() {
		_ = ln.Close()
		return nil, fmt.Errorf("inspector: bound address %s is not loopback", ln.Addr())
	}
	baseCtx, baseCancel := context.WithCancel(context.Background())
	s := &Server{store: store, transport: transport, ln: ln, host: tcpAddr.IP.String(), baseCancel: baseCancel}
	s.port = strconv.Itoa(tcpAddr.Port)
	s.srv = &http.Server{
		Handler:           s.guard(s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// URL is the address to open in a browser.
func (s *Server) URL() string { return "http://" + net.JoinHostPort(s.host, s.port) }

// Close stops the server. baseCancel runs first so every in-flight SSE handler's request context is
// cancelled and returns immediately (M8): otherwise an open browser tab keeps /api/events blocked
// and Close would wait out the full Shutdown timeout below.
func (s *Server) Close() error {
	s.baseCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// guard applies the DNS-rebinding, CSRF and security-header policy to every request.
func (s *Server) guard(next http.Handler) http.Handler {
	allowedHosts := map[string]bool{
		"127.0.0.1:" + s.port: true, "localhost:" + s.port: true, "[::1]:" + s.port: true,
		net.JoinHostPort(s.host, s.port): true, // the address actually bound (L2)
	}
	allowedOrigins := map[string]bool{}
	for h := range allowedHosts {
		allowedOrigins["http://"+h] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if !allowedHosts[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden host", http.StatusForbidden) // DNS rebinding
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// A custom header forces a CORS preflight that other origins can't pass; Origin must match.
			if r.Header.Get("X-Tuzy-Inspector") != "1" || !allowedOrigins[r.Header.Get("Origin")] {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(uiFS, "ui")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	mux.HandleFunc("GET /api/requests", s.list)
	mux.HandleFunc("GET /api/requests/{id}", s.get)
	mux.HandleFunc("GET /api/requests/{id}/curl", s.curl)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/tunnels", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, s.store.Tunnels()) })
	mux.HandleFunc("POST /api/requests/{id}/replay", s.replayOne)
	mux.HandleFunc("POST /api/replay", s.replayEdited)
	mux.HandleFunc("DELETE /api/requests", func(w http.ResponseWriter, _ *http.Request) {
		s.store.Clear()
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// writeJSON uses encoding/json, which escapes <, > and & — captured data can never form markup.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) entry(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad id"})
		return nil, false
	}
	e, ok := s.store.Get(id)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not found (evicted or cleared)"})
		return nil, false
	}
	return e, true
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, 200, s.store.List(Filter{Tunnel: q.Get("tunnel"), Method: q.Get("method"), Status: q.Get("status")}))
}

// detail is an entry plus display-decoded bodies.
type detail struct {
	*Entry
	ReqDecoded      []byte `json:"req_body_decoded,omitempty"`
	ReqDecodedTrunc bool   `json:"req_body_decoded_truncated,omitempty"`
	ResDecoded      []byte `json:"res_body_decoded,omitempty"`
	ResDecodedTrunc bool   `json:"res_body_decoded_truncated,omitempty"`
	ResEncoding     string `json:"res_encoding,omitempty"`
	DecodeError     string `json:"decode_error,omitempty"`
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	e, ok := s.entry(w, r)
	if !ok {
		return
	}
	d := detail{Entry: e}
	if dec, enc, trunc, err := decodeForDisplay(e.ResHeaders, e.ResBody); err != nil {
		d.DecodeError = err.Error()
	} else if dec != nil {
		d.ResDecoded, d.ResEncoding, d.ResDecodedTrunc = dec, enc, trunc
	}
	if dec, _, trunc, err := decodeForDisplay(e.ReqHeaders, e.ReqBody); err == nil && dec != nil {
		d.ReqDecoded, d.ReqDecodedTrunc = dec, trunc
	}
	writeJSON(w, 200, d)
}

func (s *Server) curl(w http.ResponseWriter, r *http.Request) {
	e, ok := s.entry(w, r)
	if !ok {
		return
	}
	base := "http://localhost"
	if t, ok := s.store.Tunnel(e.Tunnel); ok && t.PublicURL != "" {
		base = t.PublicURL
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, curlCommand(e, base))
}

func (s *Server) replayOne(w http.ResponseWriter, r *http.Request) {
	e, ok := s.entry(w, r)
	if !ok {
		return
	}
	if e.Kind == "ws" {
		writeJSON(w, 409, map[string]string{"error": "cannot replay a websocket entry"}) // L5
		return
	}
	if e.ReqTrunc || e.Evicted || e.ReqIncomplete {
		writeJSON(w, 409, map[string]string{"error": "the captured request body is incomplete; use Edit & Replay"})
		return
	}
	s.doReplay(w, r, fromEntry(e), e.ID)
}

// maxReplayBody bounds the edited-replay JSON envelope (body_b64 plus headers): L5.
const maxReplayBody = 8 << 20

func (s *Server) replayEdited(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxReplayBody)
	var req ReplayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "replay body too large"})
			return
		}
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}
	s.doReplay(w, r, req, 0)
}

func (s *Server) doReplay(w http.ResponseWriter, r *http.Request, req ReplayRequest, of uint64) {
	id, err := s.store.replay(r.Context(), s.transport, req, of)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]uint64{"id": id})
}

// events streams entry changes as SSE with a 15 s heartbeat.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch, cancel := s.store.Subscribe()
	defer cancel()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": ping\n\n")
		case ev, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
		}
		flusher.Flush()
	}
}
