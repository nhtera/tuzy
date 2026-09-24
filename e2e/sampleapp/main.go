//go:build e2e

// Package sampleapp is the local HTTP app the e2e suite tunnels through `tuzy`. It is started
// in-process by the test binary (see e2e/harness_test.go) rather than as a separate process, so no
// extra binary needs to be built or child process managed.
//
// Routes:
//
//	GET  /health            -- confirms the edge routed a "reserved-looking" path to the app, not
//	                            to itself (scenario: internal paths reach the local app)
//	*    /echo (or anything
//	     not matched below)  -- echoes method, path, query, headers and a sha256/length of the body
//	GET  /big?mb=N           -- streams N deterministic MiB; response header X-Body-Sha256 lets the
//	                            caller verify integrity without re-deriving the same bytes
//	GET  /sse                -- Server-Sent Events, one `data: <n>` event every 100ms
//	GET  /ws                 -- WebSocket echo (coder/websocket)
//	GET  /slow               -- writes a byte every 100ms for --duration (default 5s); used to prove
//	                            a slow visitor doesn't stall a concurrent fast one
//	POST /early413           -- responds 413 immediately without reading the request body
package sampleapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// EchoBody is what /echo (and any unmatched path) reports back to the caller.
type EchoBody struct {
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	Query      string              `json:"query"`
	Headers    map[string][]string `json:"headers"`
	BodySHA256 string              `json:"body_sha256"`
	BodyBytes  int64               `json:"body_bytes"`
}

var emptySHA256 = sha256Hex(nil)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// bigCache memoizes generated buffers so repeated /big?mb=N calls in the same test run are cheap.
var bigCache sync.Map // mb (int) -> []byte

func bigBuffer(mb int) []byte {
	if v, ok := bigCache.Load(mb); ok {
		return v.([]byte)
	}
	buf := make([]byte, mb*1<<20)
	// A fixed seed keeps this reproducible within a run; the client never needs to regenerate it
	// itself, it only compares the X-Body-Sha256 header against what it received.
	rand.New(rand.NewSource(42)).Read(buf) //nolint:gosec // test fixture, not security sensitive
	actual, _ := bigCache.LoadOrStore(mb, buf)
	return actual.([]byte)
}

func headersOf(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, v := range h {
		out[k] = v
	}
	return out
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	sum := sha256.New()
	n, err := io.Copy(sum, r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bodyHash := emptySHA256
	if n > 0 {
		bodyHash = hex.EncodeToString(sum.Sum(nil))
	}
	body := EchoBody{
		Method:     r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		Headers:    headersOf(r.Header),
		BodySHA256: bodyHash,
		BodyBytes:  n,
	}
	w.Header().Set("X-Body-Sha256", bodyHash)
	w.Header().Set("X-Body-Bytes", fmt.Sprintf("%d", n))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"app": "sampleapp", "path": r.URL.Path})
}

func bigHandler(w http.ResponseWriter, r *http.Request) {
	mb := 1
	if v := r.URL.Query().Get("mb"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &mb); err != nil || mb <= 0 {
			http.Error(w, "bad mb", http.StatusBadRequest)
			return
		}
	}
	buf := bigBuffer(mb)
	w.Header().Set("X-Body-Sha256", sha256Hex(buf))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(buf)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	ctx := r.Context()
	for n := 0; n < 600; n++ { // bounded: ~60s max even if a caller never disconnects
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprintf(w, "data: %d\n\n", n); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	ctx := context.Background()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

// slowHandler writes one byte every 100ms for the requested duration (default 5s, capped at 30s),
// keeping the response open so a concurrent request to a different path can prove it isn't blocked.
func slowHandler(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	dur := 5 * time.Second
	if v := r.URL.Query().Get("duration_ms"); v != "" {
		var ms int
		if _, err := fmt.Sscanf(v, "%d", &ms); err == nil && ms > 0 && ms <= 30_000 {
			dur = time.Duration(ms) * time.Millisecond
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	deadline := time.Now().Add(dur)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	ctx := r.Context()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(".")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// early413Handler answers 413 without reading the request body at all: the app must be able to
// reject a request before the visitor has finished (or the tunnel has finished relaying) the body.
func early413Handler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusRequestEntityTooLarge)
	_, _ = w.Write([]byte("too large"))
}

// NewHandler returns the sample app's full route table.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/big", bigHandler)
	mux.HandleFunc("/sse", sseHandler)
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/slow", slowHandler)
	mux.HandleFunc("/early413", early413Handler)
	mux.HandleFunc("/echo", echoHandler)
	mux.HandleFunc("/", echoHandler) // any other path (including "internal-looking" ones) is echoed
	return mux
}

// SortedHeaderKeys is a small helper tests use when printing headers deterministically.
func SortedHeaderKeys(h map[string][]string) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
