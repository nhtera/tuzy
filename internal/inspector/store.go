// Package inspector records traffic through the tunnel in memory and serves a local web UI
// (http://127.0.0.1:4040) to browse, replay and edit-and-replay requests. Nothing is persisted:
// bodies may contain secrets.
package inspector

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Limits (phase 6 non-functional requirements).
const (
	MaxEntries     = 500
	MaxBodyCapture = 1 << 20  // per body side
	MaxTotalBytes  = 64 << 20 // captured bytes across all entries
)

// Entry is one recorded request (or visitor WebSocket).
type Entry struct {
	ID         uint64      `json:"id"`
	Tunnel     string      `json:"tunnel"`
	StartedAt  time.Time   `json:"started_at"`
	DurationMs int64       `json:"duration_ms"`
	Kind       string      `json:"kind"` // "http" | "ws"
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	RemoteIP   string      `json:"remote_ip"`
	ReqHeaders [][2]string `json:"req_headers"`
	ReqBody    []byte      `json:"req_body"` // base64 in JSON
	ReqSize    int64       `json:"req_size"`
	ReqTrunc   bool        `json:"req_truncated"`
	// ReqIncomplete is set when the request body did not end normally on the wire (the visitor
	// aborted, or the stream was reset before REQ_END), or the captured size falls short of a
	// declared Content-Length. Unlike ReqTrunc (capped at MaxBodyCapture, the tunnel still relayed
	// the rest in full), an incomplete body is genuinely missing bytes.
	ReqIncomplete bool        `json:"req_incomplete"`
	Status        int         `json:"status"`
	ResHeaders    [][2]string `json:"res_headers"`
	ResBody       []byte      `json:"res_body"`
	ResSize       int64       `json:"res_size"`
	ResTrunc      bool        `json:"res_truncated"`
	Evicted       bool        `json:"bodies_evicted"` // bodies dropped to stay under MaxTotalBytes
	WSMsgsIn      int64       `json:"ws_msgs_in"`
	WSMsgsOut     int64       `json:"ws_msgs_out"`
	Error         string      `json:"error,omitempty"`
	ReplayOf      uint64      `json:"replay_of,omitempty"`
	Done          bool        `json:"done"`

	// wsIn/wsOut back WSMsgsIn/WSMsgsOut for a "ws" entry: the stream recorder increments them
	// lock-free (H1/M2 — never touches the store on the hot WebSocket path), and they are folded
	// into WSMsgsIn/WSMsgsOut whenever the entry is read or finalized. Shared (not copied) by
	// clone(), so a cloned/detail copy still reflects live counts; nil for "http" entries.
	wsIn, wsOut *atomic.Int64 `json:"-"`
}

// foldLive copies any live atomic counters (WebSocket message counts) into the JSON-visible
// fields. Must be called under Store.mu.
func (e *Entry) foldLive() {
	if e.wsIn != nil {
		e.WSMsgsIn = e.wsIn.Load()
	}
	if e.wsOut != nil {
		e.WSMsgsOut = e.wsOut.Load()
	}
}

// Summary is the list view of an entry (no bodies).
type Summary struct {
	ID         uint64    `json:"id"`
	Tunnel     string    `json:"tunnel"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	Kind       string    `json:"kind"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Status     int       `json:"status"`
	Error      string    `json:"error,omitempty"`
	ReplayOf   uint64    `json:"replay_of,omitempty"`
	WSMsgsIn   int64     `json:"ws_msgs_in"`
	WSMsgsOut  int64     `json:"ws_msgs_out"`
	Done       bool      `json:"done"`
}

// summary must be called under Store.mu (it folds live WS counters).
func (e *Entry) summary() Summary {
	e.foldLive()
	return Summary{
		ID: e.ID, Tunnel: e.Tunnel, StartedAt: e.StartedAt, DurationMs: e.DurationMs, Kind: e.Kind,
		Method: e.Method, Path: e.Path, Status: e.Status, Error: e.Error, ReplayOf: e.ReplayOf,
		WSMsgsIn: e.WSMsgsIn, WSMsgsOut: e.WSMsgsOut, Done: e.Done,
	}
}

// headerBytes is the memory an entry's headers hold (counted toward MaxTotalBytes: L1).
func headerBytes(hs [][2]string) int {
	n := 0
	for _, h := range hs {
		n += len(h[0]) + len(h[1])
	}
	return n
}

// capturedBodyBytes is the memory the body buffers hold, counting cap() (not len()) so append's
// growth slack is charged against the budget too (L1), and this shrinks to 0 once evicted.
func (e *Entry) capturedBodyBytes() int { return cap(e.ReqBody) + cap(e.ResBody) }

// bodyBytes is what evictLocked tracks in Store.total: captured body bytes plus header bytes.
func (e *Entry) bodyBytes() int {
	return e.capturedBodyBytes() + headerBytes(e.ReqHeaders) + headerBytes(e.ResHeaders)
}

// clone must be called under Store.mu; it snapshots small/header fields but leaves the (possibly
// large) body byte slices referencing the entry's own backing arrays. Callers that need an
// independent copy of the bodies (Get) copy them after releasing the lock (M2: don't hold the
// store lock while cloning big bodies) — safe because appendCapped only ever appends past the
// current length, never mutates already-published bytes.
func (e *Entry) clone() *Entry {
	e.foldLive()
	c := *e
	c.ReqHeaders = append([][2]string(nil), e.ReqHeaders...)
	c.ResHeaders = append([][2]string(nil), e.ResHeaders...)
	return &c
}

// Event is pushed to subscribers (SSE) when an entry is created or updated.
type Event struct {
	Type  string  `json:"type"` // "created" | "updated" | "cleared"
	Entry Summary `json:"entry"`
}

// TunnelInfo describes where a tunnel's traffic goes (used by replay and copy-as-curl).
type TunnelInfo struct {
	Name       string
	PublicURL  string
	Target     *url.URL
	HostHeader string // "auto" | "preserve" | "rewrite" (auto becomes rewrite after a switch)
	// Transport is the tunnel's own local transport (shared default for http, a per-target TLS
	// clone for https, a fileserver.RoundTripper for file://): replay uses it instead of the
	// server's default so a replayed request reaches the same place the live traffic did. Nil
	// falls back to the server's default transport (e.g. a TunnelInfo set by an older caller, or a
	// test that only cares about the http default).
	Transport http.RoundTripper
}

// Store is a bounded in-memory ring of entries with subscriber fan-out.
type Store struct {
	mu      sync.Mutex
	nextID  uint64
	order   []uint64 // oldest first
	entries map[uint64]*Entry
	total   int
	subs    map[chan Event]struct{}
	tunnels map[string]TunnelInfo
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{entries: map[uint64]*Entry{}, subs: map[chan Event]struct{}{}, tunnels: map[string]TunnelInfo{}}
}

// SetTunnel records (or updates) a tunnel's public URL and local target.
func (s *Store) SetTunnel(t TunnelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tunnels[t.Name] = t
}

// Tunnel returns what is known about a tunnel.
func (s *Store) Tunnel(name string) (TunnelInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tunnels[name]
	return t, ok
}

// add inserts e (assigning an id) and evicts to stay within limits.
func (s *Store) add(e *Entry) uint64 {
	s.mu.Lock()
	s.nextID++
	e.ID = s.nextID
	s.entries[e.ID] = e
	s.order = append(s.order, e.ID)
	s.total += e.bodyBytes()
	s.evictLocked()
	ev := Event{Type: "created", Entry: e.summary()}
	s.mu.Unlock()
	s.publish(ev)
	return e.ID
}

// update mutates entry id under the lock (if it still exists) and publishes the change. Reserved
// for the state transitions worth telling subscribers about (Response, End): H1 — publishing on
// every body chunk floods slow subscribers and drops unrelated created/done events.
func (s *Store) update(id uint64, fn func(e *Entry)) {
	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	before := e.bodyBytes()
	fn(e)
	s.total += e.bodyBytes() - before
	s.evictLocked()
	ev := Event{Type: "updated", Entry: e.summary()}
	s.mu.Unlock()
	s.publish(ev)
}

// mutate is update without the publish, for the high-frequency body hooks (every chunk). It uses
// TryLock so a busy store never blocks the proxy path (M2): on contention it returns false and the
// caller is expected to remember the drop (e.g. flag its own data truncated) and fold that in on
// its next successful call. Returns true when the entry no longer exists (nothing to drop-flag).
func (s *Store) mutate(id uint64, fn func(e *Entry)) bool {
	if !s.mu.TryLock() {
		return false
	}
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return true
	}
	before := e.bodyBytes()
	fn(e)
	s.total += e.bodyBytes() - before
	s.evictLocked()
	s.mu.Unlock()
	return true
}

// evictLocked drops the oldest entries beyond MaxEntries, then the oldest bodies beyond
// MaxTotalBytes (entries stay listed, flagged bodies_evicted).
func (s *Store) evictLocked() {
	for len(s.order) > MaxEntries {
		id := s.order[0]
		s.order = s.order[1:]
		if e, ok := s.entries[id]; ok {
			s.total -= e.bodyBytes()
			delete(s.entries, id)
		}
	}
	for _, id := range s.order {
		if s.total <= MaxTotalBytes {
			return
		}
		e := s.entries[id]
		if e.capturedBodyBytes() == 0 {
			continue
		}
		s.total -= e.capturedBodyBytes()
		e.ReqBody, e.ResBody, e.Evicted = nil, nil, true
	}
}

// Get returns a copy of an entry. The store lock is held only long enough to snapshot small fields
// and header slices; the (possibly up-to-1-MiB-per-side) bodies are copied after releasing it (M2).
func (s *Store) Get(id uint64) (*Entry, bool) {
	s.mu.Lock()
	e, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	snap := e.clone()
	reqBody, resBody := e.ReqBody, e.ResBody
	s.mu.Unlock()
	snap.ReqBody = append([]byte(nil), reqBody...)
	snap.ResBody = append([]byte(nil), resBody...)
	return snap, true
}

// Filter narrows List.
type Filter struct {
	Tunnel string
	Method string
	Status string // exact ("404") or class ("4xx")
}

func (f Filter) match(e *Entry) bool {
	if f.Tunnel != "" && e.Tunnel != f.Tunnel {
		return false
	}
	if f.Method != "" && !strings.EqualFold(e.Method, f.Method) {
		return false
	}
	if f.Status != "" {
		st := strings.ToLower(f.Status)
		code := itoa(e.Status)
		if len(st) == 3 && strings.HasSuffix(st, "xx") {
			return len(code) == 3 && code[0] == st[0]
		}
		return code == st
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// List returns summaries, newest first.
func (s *Store) List(f Filter) []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(s.order))
	for i := len(s.order) - 1; i >= 0; i-- {
		if e := s.entries[s.order[i]]; f.match(e) {
			out = append(out, e.summary())
		}
	}
	return out
}

// Tunnels returns the known tunnel names (for the filter dropdown).
func (s *Store) Tunnels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.tunnels))
	for n := range s.tunnels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Clear removes every entry.
func (s *Store) Clear() {
	s.mu.Lock()
	s.order, s.entries, s.total = nil, map[uint64]*Entry{}, 0
	s.mu.Unlock()
	s.publish(Event{Type: "cleared"})
}

// Subscribe returns a channel of events. A subscriber that falls behind is disconnected (H1): its
// channel is closed so the SSE handler returns and the browser's EventSource reconnects, resyncing
// the whole list via load() instead of silently missing events forever.
func (s *Store) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 256)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}
}

func (s *Store) publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: disconnect it rather than drop silently forever (H1)
			delete(s.subs, ch)
			close(ch)
		}
	}
}

// totalBytes is exposed for tests.
func (s *Store) totalBytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}
