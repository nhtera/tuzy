//go:build e2e

// Package e2e is a black-box test suite: it builds the real `tuzy` binary, runs a real local edge
// (`wrangler dev` against local D1) and a real sample app, then drives them exactly like a user
// would (real HTTP/WebSocket clients, a real CLI subprocess).
//
// Run it with:
//
//	go test -tags e2e -count=1 ./e2e/...
//
// Requirements: Node.js + npm deps installed in edge/ (`cd edge && npm install`), and Go able to
// build ./cmd/tuzy. Ports are fixed and the suite fails fast (does not silently pick another port)
// if one is busy: TUZY_E2E_EDGE_PORT (default 8788; 8787 is commonly taken by other local tools)
// and TUZY_E2E_APP_PORT (default 3000). Set TUZY_E2E_SLOW=1 to additionally run the long-stream
// budget-cut scenario, which is slow by construction (LONG_STREAM_AFTER_SECONDS=300 in the dev
// config) and skipped by default.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/nhtera/tuzy/e2e/sampleapp"
)

const (
	defaultEdgePort = 8788
	defaultAppPort  = 3000

	healthTimeout  = 45 * time.Second
	onlineTimeout  = 20 * time.Second
	shutdownGrace  = 5 * time.Second
	tokenPrefix    = "tzy_"
	tokenRandBytes = 32 // -> 43 base64url chars, matching edge/src/lib/tokens.ts
)

// harness owns every long-lived process/resource the suite shares (TestMain sets `h`).
type harness struct {
	repoRoot string
	edgeDir  string
	tmpRoot  string // parent of all per-run temp dirs (CLI binary, configs, tls fixtures)
	cliPath  string

	edgePort int
	appPort  int

	edgeCmd *exec.Cmd
	edgeLog *os.File

	appSrv *http.Server
	appLn  net.Listener

	client *http.Client // no redirects, generous timeout; callers set req.Host
}

var h *harness

func TestMain(m *testing.M) {
	hh, err := setupHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: setup failed:", err)
		os.Exit(1)
	}
	h = hh
	code := m.Run()
	h.teardown()
	os.Exit(code)
}

func intEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// setupHarness brings up every shared dependency once for the whole suite (TestMain, not per
// test): a from-scratch local D1 + wrangler dev edge, the sample app, and the built CLI binary.
func setupHarness() (*harness, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("could not locate e2e package directory")
	}
	repoRoot := filepath.Dir(filepath.Dir(thisFile))
	edgeDir := filepath.Join(repoRoot, "edge")

	hh := &harness{
		repoRoot: repoRoot,
		edgeDir:  edgeDir,
		edgePort: intEnv("TUZY_E2E_EDGE_PORT", defaultEdgePort),
		appPort:  intEnv("TUZY_E2E_APP_PORT", defaultAppPort),
		client: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	tmpRoot, err := os.MkdirTemp("", "tuzy-e2e-")
	if err != nil {
		return nil, err
	}
	hh.tmpRoot = tmpRoot

	if err := requireFreePort(hh.edgePort, "TUZY_E2E_EDGE_PORT"); err != nil {
		return nil, err
	}
	if err := requireFreePort(hh.appPort, "TUZY_E2E_APP_PORT"); err != nil {
		return nil, err
	}

	// Fresh local D1/DO/KV state every run: deterministic, and lets the suite be re-run 3x in a
	// row without accumulating names/tokens/rate-limit state from the previous run.
	if err := os.RemoveAll(filepath.Join(edgeDir, ".wrangler", "state")); err != nil {
		return nil, fmt.Errorf("clearing .wrangler/state: %w", err)
	}

	if out, err := runIn(edgeDir, "node", "scripts/gen-dev-config.mjs"); err != nil {
		return nil, fmt.Errorf("gen-dev-config: %w\n%s", err, out)
	}

	wranglerBin := filepath.Join(edgeDir, "node_modules", ".bin", "wrangler")
	if _, err := os.Stat(wranglerBin); err != nil {
		return nil, fmt.Errorf("wrangler not installed (%s): run `cd edge && npm install`: %w", wranglerBin, err)
	}
	if out, err := runIn(edgeDir, "node", wranglerBin, "d1", "migrations", "apply", "tuzy", "--local", "-c", "wrangler.dev.gen.jsonc"); err != nil {
		return nil, fmt.Errorf("d1 migrations apply: %w\n%s", err, out)
	}

	if err := hh.startEdge(wranglerBin); err != nil {
		return nil, err
	}
	if err := waitHealth(hh.edgePort, healthTimeout); err != nil {
		hh.teardown()
		return nil, err
	}

	cliPath, err := buildCLI(repoRoot, tmpRoot)
	if err != nil {
		hh.teardown()
		return nil, err
	}
	hh.cliPath = cliPath

	if err := hh.startSampleApp(); err != nil {
		hh.teardown()
		return nil, err
	}

	return hh, nil
}

// requireFreePort fails fast (never silently picks another port) when the fixed port is busy.
func requireFreePort(port int, envVar string) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("port %d is busy (set %s to use a different one): %w", port, envVar, err)
	}
	return ln.Close()
}

func runIn(dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

// startEdge launches `wrangler dev` in its own process group so teardown can kill it and every
// child (workerd) it spawns without leaving ghosts.
func (hh *harness) startEdge(wranglerBin string) error {
	logPath := filepath.Join(hh.tmpRoot, "wrangler-dev.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	hh.edgeLog = logFile

	// The interstitial secret normally comes from the gitignored edge/.dev.vars; CI has none, and a
	// missing secret makes /__tuzy/continue fail closed. Pass a fixed test value explicitly.
	cmd := exec.Command("node", wranglerBin, "dev", "-c", "wrangler.dev.gen.jsonc", "--port", strconv.Itoa(hh.edgePort),
		"--var", "INTERSTITIAL_SECRET:e2e-interstitial-secret")
	cmd.Dir = hh.edgeDir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting wrangler dev: %w", err)
	}
	hh.edgeCmd = cmd
	return nil
}

func waitHealth(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/v1/health", port), nil)
		req.Host = "localhost"
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("edge did not become healthy within %s: %w (see wrangler-dev.log)", timeout, lastErr)
}

// buildCLI compiles ./cmd/tuzy once for the whole suite.
func buildCLI(repoRoot, tmpRoot string) (string, error) {
	bin := filepath.Join(tmpRoot, "tuzy")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./cmd/tuzy")
	cmd.Dir = repoRoot
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build ./cmd/tuzy: %w\n%s", err, buf.String())
	}
	return bin, nil
}

func (hh *harness) startSampleApp() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hh.appPort))
	if err != nil {
		return fmt.Errorf("sample app port %d busy: %w", hh.appPort, err)
	}
	hh.appLn = ln
	hh.appSrv = &http.Server{Handler: sampleapp.NewHandler()}
	go func() { _ = hh.appSrv.Serve(ln) }()
	return nil
}

// teardown stops every process/listener the harness owns. It never leaves a ghost wrangler/workerd
// process behind: the edge is killed by process GROUP (wrangler dev spawns node -> workerd
// children that inherit the group), first gracefully (SIGTERM) then forcefully (SIGKILL).
func (hh *harness) teardown() {
	if hh.appSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = hh.appSrv.Shutdown(ctx)
		cancel()
	}
	if hh.edgeCmd != nil && hh.edgeCmd.Process != nil {
		killProcessGroup(hh.edgeCmd, shutdownGrace)
	}
	if hh.edgeLog != nil {
		_ = hh.edgeLog.Close()
	}
	if keep := os.Getenv("TUZY_E2E_KEEP_EDGE_LOG"); keep != "" && hh.tmpRoot != "" {
		// Debugging: keep a copy of the wrangler dev output.
		if b, err := os.ReadFile(filepath.Join(hh.tmpRoot, "wrangler-dev.log")); err == nil {
			_ = os.WriteFile(keep, b, 0o600)
		}
	}
	if hh.tmpRoot != "" {
		_ = os.RemoveAll(hh.tmpRoot)
	}
}

// killProcessGroup sends SIGTERM to the whole process group, waits up to grace, then SIGKILLs
// anything still alive (belt-and-braces: also SIGKILLs the leader pid directly in case Setpgid
// didn't take, e.g. if the process already exited).
func killProcessGroup(cmd *exec.Cmd, grace time.Duration) {
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		if pgid, err := syscall.Getpgid(pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = cmd.Process.Kill()
		<-done
	}
}

// --- small shared crypto helpers (kept here so seeding and token generation agree with
// edge/src/lib/tokens.ts and ids.ts without importing TypeScript into Go). ---

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// newToken mirrors edge/src/lib/tokens.ts generateToken(): "tzy_" + 32 random bytes, base64url.
func newToken() string {
	b := make([]byte, tokenRandBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return tokenPrefix + base64URLNoPad(b)
}

func base64URLNoPad(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out bytes.Buffer
	for i := 0; i < len(b); i += 3 {
		chunk := b[i:minInt(i+3, len(b))]
		n := len(chunk)
		var v uint32
		for _, c := range chunk {
			v = v<<8 | uint32(c)
		}
		v <<= uint(8 * (3 - n))
		nChars := []int{0, 2, 3, 4}[n]
		for j := 0; j < nChars; j++ {
			shift := 18 - 6*j
			out.WriteByte(alphabet[(v>>uint(shift))&0x3f])
		}
	}
	return out.String()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func nowUnix() int64 { return time.Now().Unix() }

// waitFor polls fn until it returns true or the timeout elapses.
func waitFor(t *testing.T, timeout, interval time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(interval)
	}
	return fn()
}
