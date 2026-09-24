//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// --- D1 seeding -------------------------------------------------------------------------------

// d1Exec runs `wrangler d1 execute tuzy --local` with an inline --command against the shared dev
// database. It is only used for seeding/asserting DB state the product API has no endpoint for
// (e.g. granting the admin role, or pre-loading usage_monthly for the budget-cut scenario).
func (hh *harness) d1Exec(t *testing.T, sql string) string {
	t.Helper()
	wranglerBin := filepath.Join(hh.edgeDir, "node_modules", ".bin", "wrangler")
	out, err := runIn(hh.edgeDir, "node", wranglerBin, "d1", "execute", "tuzy", "--local", "-c", "wrangler.dev.gen.jsonc", "--command", sql)
	if err != nil {
		t.Fatalf("d1 execute failed: %v\n%s\nsql: %s", err, out, sql)
	}
	return out
}

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// testUser is one seeded account + its full-scope token.
type testUser struct {
	ID    string
	Email string
	Token string
}

type seedOpts struct {
	admin     bool
	maxNames  int // 0 = default (10)
	usageSecs int64
}

// seedUser inserts a fresh user + full-scope token directly into D1 (the OTP login flow itself is
// covered by the edge's own unit tests, so tests here start from a token like a CI user would).
func (hh *harness) seedUser(t *testing.T, label string, opts seedOpts) testUser {
	t.Helper()
	suffix := randomHex(8)
	u := testUser{
		ID:    "usr_" + suffix + "00000000",
		Email: fmt.Sprintf("e2e-%s-%s@example.test", label, suffix),
		Token: newToken(),
	}
	now := nowUnix()
	role := "user"
	if opts.admin {
		role = "admin"
	}
	maxNames := 10
	if opts.maxNames > 0 {
		maxNames = opts.maxNames
	}
	var b strings.Builder
	fmt.Fprintf(&b, "INSERT INTO users (id,email,role,max_names,created_at) VALUES (%s,%s,%s,%d,%d);",
		sqlQuote(u.ID), sqlQuote(u.Email), sqlQuote(role), maxNames, now)
	fmt.Fprintf(&b, "INSERT INTO tokens (id,user_id,token_hash,scope,label,created_at,last_used_at) VALUES (%s,%s,%s,'full',%s,%d,%d);",
		sqlQuote("tok_"+suffix), sqlQuote(u.ID), sqlQuote(sha256Hex(u.Token)), sqlQuote(label), now, now)
	if opts.usageSecs > 0 {
		month := time.Now().UTC().Format("2006-01")
		fmt.Fprintf(&b, "INSERT INTO usage_monthly (user_id,month,long_stream_seconds) VALUES (%s,%s,%d);",
			sqlQuote(u.ID), sqlQuote(month), opts.usageSecs)
	}
	hh.d1Exec(t, b.String())
	return u
}

// --- visitor HTTP -------------------------------------------------------------------------------

// edgeURL builds a request to the edge's fixed local port; callers set the virtual Host explicitly
// (never real DNS) exactly like a browser reaching <name>.tuzy.dev would: the connection always
// goes to 127.0.0.1:<edgePort>, the Host header decides which tunnel/app the edge routes to.
func (hh *harness) edgeURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", hh.edgePort, path)
}

// tunnelHost is the Host header for a tunnel name in the dev config (BASE_DOMAIN=localhost).
func (hh *harness) tunnelHost(name string) string {
	return fmt.Sprintf("%s.localhost:%d", name, hh.edgePort)
}

func (hh *harness) apexHost() string {
	return fmt.Sprintf("localhost:%d", hh.edgePort)
}

// newVisitorRequest builds a request that reaches the edge on its fixed local port with an
// explicit Host header (see edgeURL doc).
func (hh *harness) newVisitorRequest(t *testing.T, method, host, path string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, hh.edgeURL(path), body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	return req
}

// --- CLI subprocess -----------------------------------------------------------------------------

// cliEnv is the base environment every `tuzy` invocation gets: a local dev server, a token (so
// resolveEnv never touches the OS keychain), and no update check.
func (hh *harness) cliEnv(token string) []string {
	return []string{
		"HOME=" + os.Getenv("HOME"),
		"PATH=" + os.Getenv("PATH"),
		"TUZY_SERVER=" + fmt.Sprintf("http://localhost:%d", hh.edgePort),
		"TUZY_TOKEN=" + token,
		"TUZY_NO_UPDATE_CHECK=1",
	}
}

// runCLIOnce runs a short-lived `tuzy` command to completion and returns its combined output.
func (hh *harness) runCLIOnce(t *testing.T, timeout time.Duration, token string, args ...string) (string, error) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	full := append([]string{"--config", configPath}, args...)
	cmd := exec.CommandContext(ctx, hh.cliPath, full...)
	cmd.Env = hh.cliEnv(token)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// cliProc is a long-running `tuzy http` / `tuzy start` subprocess (an "agent").
type cliProc struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuffer
	once sync.Once
}

// startCLI starts a long-running tuzy invocation (e.g. `http`, `start --all`) in its own process
// group, in dir (default: a fresh temp dir), with token as TUZY_TOKEN.
func (hh *harness) startCLI(t *testing.T, dir, token string, args ...string) *cliProc {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	configPath := filepath.Join(t.TempDir(), "config.toml")
	cmd := exec.Command(hh.cliPath, append([]string{"--config", configPath}, args...)...)
	cmd.Dir = dir
	cmd.Env = hh.cliEnv(token)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	buf := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = buf, buf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %v: %v", args, err)
	}
	p := &cliProc{t: t, cmd: cmd, out: buf}
	t.Cleanup(p.stop)
	return p
}

// stop gracefully interrupts the process (matching a user's Ctrl-C) and, if it doesn't exit
// quickly, kills its whole process group. Safe to call more than once.
func (p *cliProc) stop() {
	p.once.Do(p.doStop)
}

func (p *cliProc) doStop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = p.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		killProcessGroup(p.cmd, 2*time.Second)
	}
}

// wait blocks until the process exits (e.g. after a terminal GOAWAY) and returns its error.
func (p *cliProc) wait(timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("process did not exit within %s (output so far:\n%s)", timeout, p.out.String())
	}
}

func (p *cliProc) String() string { return p.out.String() }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// --- misc -----------------------------------------------------------------------------------

// waitOnline polls a tunnel host until the edge stops answering "offline" (502 with x-tuzy-edge),
// i.e. until an agent has connected and the KV marker is visible.
func (hh *harness) waitOnline(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	ok := waitFor(t, timeout, 200*time.Millisecond, func() bool {
		req := hh.newVisitorRequest(t, http.MethodGet, hh.tunnelHost(name), "/health", nil)
		resp, err := hh.client.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.Header.Get("X-Tuzy-Edge") == "" // an edge status page always sets this; the app never does
	})
	if !ok {
		t.Fatalf("tunnel %q did not come online within %s", name, timeout)
	}
}
