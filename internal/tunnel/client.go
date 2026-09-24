package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// Options configures one tunnel (one name → one local target).
type Options struct {
	Server     *url.URL // e.g. https://tuzy.dev
	Token      string
	Name       string
	Force      bool
	Target     *url.URL // e.g. http://localhost:3000
	HostHeader string   // "auto" (default) | "preserve" | "rewrite"
	UserAgent  string   // e.g. "tuzy/1.0.0 darwin/arm64"
	OnEvent    func(Event)
	OnAccess   func(AccessEntry)
	Recorder   Recorder // optional: local inspector
	Logger     *slog.Logger

	// Tunables (zero = default); tests shrink them.
	PingInterval     time.Duration // 15 s
	PongTimeout      time.Duration // 10 s
	ProbeTimeout     time.Duration // 3 s: liveness deadline after sleep / network change
	ProbeEvery       time.Duration // 2 s: how often to check for sleep / network change
	CreditTimeout    time.Duration // 30 s
	DrainGrace       time.Duration // 5 s
	HandshakeTimeout time.Duration // 10 s
	// DialHTTPClient carries the connect WebSocket (default: env proxy aware).
	DialHTTPClient *http.Client
	// LocalTransport reaches the local target (default: no proxy, no compression).
	LocalTransport http.RoundTripper
}

// EventKind describes a tunnel state change.
type EventKind int

// Tunnel lifecycle events.
const (
	EventConnecting   EventKind = iota // dialing the edge
	EventOnline                        // READY received
	EventReconnecting                  // connection lost or dial failed; retrying after RetryIn
	EventRenamed                       // GOAWAY renamed: reconnecting as the new name
	EventHostRewrite                   // auto Host mode: the dev server rejected the public Host; now rewriting
)

// Event is reported through Options.OnEvent.
type Event struct {
	Kind    EventKind
	Name    string
	URL     string
	Epoch   int64
	Err     error
	RetryIn time.Duration
	OldName string
}

// ExitError is a terminal failure: the CLI prints Message and exits 1.
type ExitError struct {
	Status  int    // HTTP status from connect, 0 for GOAWAY
	Code    string // server error code or GOAWAY reason
	Message string
}

func (e *ExitError) Error() string { return e.Message }

// apiErrorBody is the JSON error shape of the edge API: {"error": {"code", "message", ...}}.
type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		NewName string `json:"new_name"`
	} `json:"error"`
}

// NewLocalTransport is the default transport used to reach a local target: no proxy (it's on the
// same machine), no compression (so bytes relayed to the visitor match what the local app sent), a
// small idle-connection pool. Shared by the tunnel client's default and the inspector's replay so
// both paths behave identically.
func NewLocalTransport() *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
}

// NewInstanceID returns a random per-process id (22 chars of base64url).
func NewInstanceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// Client keeps one tunnel connected, reconnecting with the same instance id.
type Client struct {
	mu         sync.Mutex // guards opts.Name / opts.Force (changed by Run)
	opts       Options
	instanceID string
	backoff    *backoff
	cfg        *sessionConfig
	onSession  func(*session) // test hook: observe each new session
}

// NewClient validates options and applies defaults.
func NewClient(opts Options) (*Client, error) {
	if opts.Server == nil || opts.Target == nil || opts.Name == "" {
		return nil, errors.New("tunnel: server, target and name are required")
	}
	switch opts.HostHeader {
	case "":
		opts.HostHeader = "auto"
	case "auto", "preserve", "rewrite":
	default:
		return nil, fmt.Errorf("tunnel: host header mode %q (want auto, preserve or rewrite)", opts.HostHeader)
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&opts.PingInterval, 15*time.Second)
	def(&opts.PongTimeout, 10*time.Second)
	def(&opts.ProbeTimeout, 3*time.Second)
	def(&opts.ProbeEvery, 2*time.Second)
	def(&opts.CreditTimeout, 30*time.Second)
	def(&opts.DrainGrace, 5*time.Second)
	def(&opts.HandshakeTimeout, 10*time.Second)
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.DialHTTPClient == nil {
		opts.DialHTTPClient = &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
			// Never follow redirects: Go would replay the Authorization header to the new URL.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if opts.LocalTransport == nil {
		opts.LocalTransport = NewLocalTransport()
	}
	c := &Client{
		opts:       opts,
		instanceID: NewInstanceID(),
		backoff:    newBackoff(),
		cfg: &sessionConfig{
			target:        opts.Target,
			hostHeader:    opts.HostHeader,
			transport:     opts.LocalTransport,
			pingInterval:  opts.PingInterval,
			pongTimeout:   opts.PongTimeout,
			probeTimeout:  opts.ProbeTimeout,
			probeEvery:    opts.ProbeEvery,
			creditTimeout: opts.CreditTimeout,
			drainGrace:    opts.DrainGrace,
			onAccess:      opts.OnAccess,
			recorder:      opts.Recorder,
			logger:        opts.Logger,
		},
	}
	// Auto Host mode tells the user once when it switched to rewriting.
	c.cfg.onHostSwitch = func() { c.emit(Event{Kind: EventHostRewrite, Name: c.Name()}) }
	return c, nil
}

// InstanceID is stable for the life of the process (and across reconnects).
func (c *Client) InstanceID() string { return c.instanceID }

// Name is the current tunnel name (it changes after GOAWAY renamed).
func (c *Client) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opts.Name
}

// minHealthyUptime: a session must last this long before the backoff resets, so an edge that
// accepts then drops us immediately doesn't cause a tight reconnect loop.
const minHealthyUptime = 30 * time.Second

func (c *Client) emit(e Event) {
	if c.opts.OnEvent != nil {
		c.opts.OnEvent(e)
	}
}

// Run connects and serves until ctx is cancelled (graceful DRAIN → nil) or a terminal error.
func (c *Client) Run(ctx context.Context) error {
	attempt, renameHops := 0, 0
	var delay time.Duration
	for {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		c.emit(Event{Kind: EventConnecting, Name: c.Name()})
		sess, ready, err := c.connect(ctx)
		if err != nil {
			var moved *renamedError
			if errors.As(err, &moved) {
				// The name was renamed while we were offline: follow the hint like GOAWAY renamed,
				// bounded so a hint cycle can't loop forever.
				renameHops++
				if renameHops > maxRenameHops {
					return moved.exit
				}
				c.follow(moved.newName)
				delay = 0
				continue
			}
			var exit *ExitError
			if errors.As(err, &exit) {
				return exit
			}
			if ctx.Err() != nil {
				return nil
			}
			var rerr *retryError
			delay = c.backoff.delay(attempt)
			if errors.As(err, &rerr) && rerr.after > delay {
				delay = rerr.after
			}
			if isNoHello(err) {
				// The edge resets a tunnel object whose agent socket never delivered HELLO (it wedges
				// after some deploys), so a prompt retry reaches a fresh one: jittered like a restart,
				// without growing the backoff.
				delay = c.backoff.restart()
			} else {
				attempt++
			}
			c.emit(Event{Kind: EventReconnecting, Name: c.Name(), Err: err, RetryIn: delay})
			continue
		}

		renameHops = 0
		c.mu.Lock()
		c.opts.Force = false // --force takes over once; reconnects must not evict a legitimate new holder
		c.mu.Unlock()
		c.emit(Event{Kind: EventOnline, Name: ready.Name, URL: ready.URL, Epoch: ready.Epoch})
		started := time.Now()
		runErr := sess.run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) >= minHealthyUptime {
			attempt = 0
		}
		if g := sess.goawayMsg(); g != nil {
			if exit := goawayExit(g); exit != nil {
				return exit
			}
			if g.Reason == protocol.ReasonRenamed && g.NewName != "" {
				c.follow(g.NewName)
				delay = 0
				continue
			}
		}
		delay = c.backoff.delay(attempt)
		attempt++
		if websocket.CloseStatus(runErr) == websocket.StatusServiceRestart {
			delay = c.backoff.restart()
		}
		c.emit(Event{Kind: EventReconnecting, Name: c.Name(), Err: runErr, RetryIn: delay})
	}
}

// maxRenameHops bounds how many 410 rename hints one connect attempt follows.
const maxRenameHops = 3

// follow switches to a new name (the old one was renamed) and reports it.
func (c *Client) follow(newName string) {
	c.mu.Lock()
	old := c.opts.Name
	c.opts.Name = newName
	c.mu.Unlock()
	c.emit(Event{Kind: EventRenamed, Name: newName, OldName: old})
}

// renamedError is a 410 carrying a rename hint; exit is returned once the hop budget is spent.
type renamedError struct {
	newName string
	exit    *ExitError
}

func (e *renamedError) Error() string { return e.exit.Error() }

// retryError is a transient connect failure (network, 429, 5xx).
type retryError struct {
	err   error
	after time.Duration
}

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

// connect dials, performs HELLO → READY and returns a ready session.
func (c *Client) connect(ctx context.Context) (*session, *protocol.ReadyMsg, error) {
	u := *c.opts.Server
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/api/v1/connect"
	c.mu.Lock()
	name, force := c.opts.Name, c.opts.Force
	c.mu.Unlock()
	q := url.Values{"name": {name}, "instance": {c.instanceID}}
	if force {
		q.Set("force", "1")
	}
	u.RawQuery = q.Encode()

	hdr := http.Header{}
	if c.opts.Token != "" {
		hdr.Set("Authorization", "Bearer "+c.opts.Token)
	}
	if c.opts.UserAgent != "" {
		hdr.Set("User-Agent", c.opts.UserAgent)
	}
	dialCtx, cancel := context.WithTimeout(ctx, c.opts.HandshakeTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{HTTPClient: c.opts.DialHTTPClient, HTTPHeader: hdr})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, nil, c.statusError(resp)
		}
		return nil, nil, &retryError{err: fmt.Errorf("connect %s: %w", c.opts.Server.Host, err)}
	}

	s := newSession(conn, c.cfg, name)
	if c.onSession != nil {
		c.onSession(s)
	}
	ready, goaway, err := s.handshake(ctx, protocol.HelloMsg{Proto: 1, Client: c.opts.UserAgent, InstanceID: c.instanceID}, c.opts.HandshakeTimeout)
	if err != nil {
		_ = conn.CloseNow()
		return nil, nil, &retryError{err: fmt.Errorf("handshake: %w", err)}
	}
	if goaway != nil {
		_ = conn.CloseNow()
		if exit := goawayExit(goaway); exit != nil {
			return nil, nil, exit
		}
		return nil, nil, &retryError{err: fmt.Errorf("edge sent GOAWAY %s", goaway.Reason)}
	}
	return s, ready, nil
}

// statusError maps a connect HTTP status onto the behaviour contract (phase 3 table).
func (c *Client) statusError(resp *http.Response) error {
	var env apiErrorBody
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_ = json.Unmarshal(b, &env)
	body := env.Error
	msg := body.Message
	if msg == "" {
		msg = resp.Status
	}
	exit := func(m string) error { return &ExitError{Status: resp.StatusCode, Code: body.Code, Message: m} }
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusForbidden:
		return exit(msg)
	case http.StatusUnauthorized:
		if body.Code == "token_expired" {
			return exit("session expired: run `tuzy login`")
		}
		return exit("not logged in: run `tuzy login`")
	case http.StatusNotFound:
		if body.Code == "name_not_reserved" {
			return exit(fmt.Sprintf("you don't own `%s`: run `tuzy names add %s`", c.Name(), c.Name()))
		}
		return exit(msg)
	case http.StatusConflict:
		// Most often a second terminal on the same machine reusing the default name, so offer both fixes.
		return exit(fmt.Sprintf("`%[1]s` is already live in another tuzy process (another terminal or device)\n"+
			"  • to run another tunnel alongside it, use a different name: --name <other> (`tuzy names ls` lists yours)\n"+
			"  • to move `%[1]s` here, rerun with --force (the other process disconnects)", c.Name()))
	case http.StatusGone:
		if body.NewName != "" {
			return &renamedError{
				newName: body.NewName,
				exit:    &ExitError{Status: resp.StatusCode, Code: body.Code, Message: fmt.Sprintf("`%s` was renamed to `%s`", c.Name(), body.NewName)},
			}
		}
		return exit(fmt.Sprintf("`%s` was removed", c.Name()))
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return &retryError{err: fmt.Errorf("server busy (%s)", resp.Status), after: retryAfter(resp.Header, time.Now())}
	}
	if resp.StatusCode >= 500 {
		return &retryError{err: fmt.Errorf("server error (%s)", resp.Status), after: retryAfter(resp.Header, time.Now())}
	}
	return exit(msg)
}

// goawayExit returns the terminal error for GOAWAY reasons that must not reconnect (§6).
func goawayExit(g *protocol.GoawayMsg) error {
	msg := func(def string) string {
		if g.Message != "" {
			return g.Message
		}
		return def
	}
	switch g.Reason {
	case protocol.ReasonUpgradeRequired:
		return &ExitError{Code: g.Reason, Message: msg("this tuzy version is no longer supported") + ": run `tuzy update` or reinstall from https://tuzy.dev"}
	case protocol.ReasonReplaced:
		return &ExitError{Code: g.Reason, Message: msg("another device took over this tunnel")}
	case protocol.ReasonDeleted:
		return &ExitError{Code: g.Reason, Message: msg("this tunnel name was removed")}
	case protocol.ReasonSuspended:
		return &ExitError{Code: g.Reason, Message: msg("this tunnel was suspended") + " (contact abuse@tuzy.dev)"}
	case protocol.ReasonRevoked:
		return &ExitError{Code: g.Reason, Message: msg("this session was logged out") + ": run `tuzy login`"}
	}
	return nil
}

// isNoHello reports a handshake the edge closed 1002 "no HELLO": the tunnel object never received
// our HELLO and resets itself (edge ≥ edge-v1.1.6).
func isNoHello(err error) bool {
	var ce websocket.CloseError
	return errors.As(err, &ce) && ce.Code == websocket.StatusProtocolError && ce.Reason == "no HELLO"
}
