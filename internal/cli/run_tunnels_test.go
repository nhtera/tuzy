package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/config"
	"github.com/nhtera/tuzy/internal/logging"
	"github.com/nhtera/tuzy/internal/tunnel"
	"github.com/nhtera/tuzy/internal/ui"
	"github.com/nhtera/tuzy/internal/upstream"
)

// L11: --upstream-sni / --upstream-insecure (or a tuzy.toml upstream_sni / upstream_insecure) only
// mean anything for an https:// target; buildTarget must refuse rather than silently ignore them for
// any other kind.
func TestBuildTargetRejectsUpstreamTLSOptsForNonHTTPSTarget(t *testing.T) {
	dir := t.TempDir()
	addrs := []string{"3000", "http://localhost:3000", "file://" + filepath.ToSlash(dir)}
	for _, addr := range addrs {
		var out bytes.Buffer
		if _, _, _, err := buildTarget(addr, upstream.TLSOptions{SNI: "example.internal"}, upstream.ResolveCLIDir, &out); err == nil {
			t.Errorf("buildTarget(%q, SNI set): expected an error", addr)
		}
		if _, _, _, err := buildTarget(addr, upstream.TLSOptions{Insecure: true}, upstream.ResolveCLIDir, &out); err == nil {
			t.Errorf("buildTarget(%q, Insecure set): expected an error", addr)
		}
	}
	// An https:// target must still accept them.
	var out bytes.Buffer
	if _, _, _, err := buildTarget("https://localhost:8443", upstream.TLSOptions{Insecure: true}, upstream.ResolveCLIDir, &out); err != nil {
		t.Fatalf("https target with Insecure set: %v", err)
	}
}

// L4: the tunnelSpec.target passed to BuildLocalRequest/ws_stream stays the fileTargetScheme
// placeholder (tunnel.FileTarget()) for a file:// target, but the display string shown to a human
// (and logged) names the actual served directory.
func TestBuildTargetFileDisplayNamesTheServedDirectory(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	target, display, transport, err := buildTarget("file://"+filepath.ToSlash(dir), upstream.TLSOptions{}, upstream.ResolveCLIDir, &out)
	t.Cleanup(func() { closeTransport(transport) }) // Windows: release the dir handle
	if err != nil {
		t.Fatal(err)
	}
	if target.String() != tunnel.FileTarget().String() {
		t.Fatalf("target = %s, want the FileTarget placeholder %s", target, tunnel.FileTarget())
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := "file://" + resolvedDir; display != want {
		t.Fatalf("display = %q, want %q", display, want)
	}
	if transport == nil {
		t.Fatal("expected a fileserver transport")
	}
	if !strings.Contains(out.String(), "serving files from") {
		t.Fatalf("missing startup notice: %q", out.String())
	}
}

func newTestEnv(user config.User) *runtimeEnv {
	// serverSource "default" keeps runtimeEnv.displayServer() (called by logFlags.output) from
	// dereferencing a nil server.
	return &runtimeEnv{serverSource: "default", user: user}
}

// M1: when --log (or the user config's log) sends structured logs to stdout, every other startup
// notice (buildTarget's "serving files from …" / --upstream-insecure warning, startInspector's
// "inspector: …") must move to stderr instead, so a caller parsing stdout as a JSON stream never
// sees a plain line.
func TestLogFlagsOutputRoutesNoticesToStderrWhenLogIsStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	l := logFlags{dest: "stdout", format: "json"}
	_, lg, notices, err := l.output(cmd, newTestEnv(config.User{}), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	if notices != cmd.ErrOrStderr() {
		t.Fatal("notices must be cmd.ErrOrStderr() when the log destination is stdout")
	}
}

func TestLogFlagsOutputKeepsNoticesOnStdoutWhenLoggingIsOff(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	var l logFlags // no --log/--log-format/--log-level at all
	_, lg, notices, err := l.output(cmd, newTestEnv(config.User{}), true)
	if err != nil {
		t.Fatal(err)
	}
	if lg != nil {
		t.Fatal("expected no logger when --log is never set")
	}
	if notices != cmd.OutOrStdout() {
		t.Fatal("notices must stay on stdout when logging is off")
	}
}

func TestLogFlagsOutputKeepsNoticesOnStdoutWhenLogIsStderr(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	l := logFlags{dest: "stderr"}
	_, lg, notices, err := l.output(cmd, newTestEnv(config.User{}), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	if notices != cmd.OutOrStdout() {
		t.Fatal("notices must stay on stdout when the log destination is stderr, not stdout")
	}
}

// L11: --log-format / --log-level without --log (and no log destination configured) have no
// effect; the user should be told so on stderr instead of silently doing nothing.
func TestLogFlagsOutputNotesFormatOrLevelWithoutLog(t *testing.T) {
	for _, l := range []logFlags{{format: "json"}, {level: "debug"}} {
		var out, errOut bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		if _, _, _, err := l.output(cmd, newTestEnv(config.User{}), true); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(errOut.String(), "no effect without --log") {
			t.Fatalf("logFlags %+v: expected a note on stderr, got %q", l, errOut.String())
		}
	}
}

func TestLogFlagsOutputNoNoteWhenLogSetViaConfig(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	l := logFlags{format: "json"} // no --log flag, but the user config sets one below
	_, lg, _, err := l.output(cmd, newTestEnv(config.User{Log: "stderr"}), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	if strings.Contains(errOut.String(), "no effect without --log") {
		t.Fatalf("unexpected note: the user config already sets a log destination: %q", errOut.String())
	}
}

func TestLogFlagsOutputNoNoteWhenLogAlsoSet(t *testing.T) {
	var out, errOut bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	l := logFlags{dest: "stderr", format: "json"}
	_, lg, _, err := l.output(cmd, newTestEnv(config.User{}), true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()
	if strings.Contains(errOut.String(), "no effect without --log") {
		t.Fatalf("unexpected note: --log is set: %q", errOut.String())
	}
}

// L11 end-to-end: `tuzy http`/`tuzy start` must refuse --upstream-sni / --upstream-insecure (resp.
// a tuzy.toml upstream_sni / upstream_insecure entry) for a non-https target rather than silently
// ignoring it.
func TestHTTPRejectsUpstreamTLSFlagsForNonHTTPSTarget(t *testing.T) {
	t.Setenv("TUZY_TOKEN", "tzy_x")
	_, err := run(t, "", "http", "3000", "--name", "shop", "--upstream-insecure", "--server", "http://localhost:1")
	if err == nil || !strings.Contains(err.Error(), "only apply to an https") {
		t.Fatalf("err = %v", err)
	}
}

func TestStartRejectsUpstreamTLSOptsForNonHTTPSAddr(t *testing.T) {
	t.Setenv("TUZY_TOKEN", "tzy_x")
	dir := t.TempDir()
	proj := filepath.Join(dir, "tuzy.toml")
	if err := os.WriteFile(proj, []byte("[tunnels.web]\naddr = \"3000\"\nupstream_insecure = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := run(t, "", "start", "--all", "--file", proj, "--server", "http://localhost:1")
	if err == nil || !strings.Contains(err.Error(), "only apply to an https") {
		t.Fatalf("err = %v", err)
	}
}

// L6: a client's terminal error (Run returns non-nil) must reach the structured log before runTunnels
// cancels the other tunnels, not just the process's final non-zero exit.
func TestRunTunnelsLogsTerminalClientError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 403: statusError maps this to a terminal *ExitError (no reconnect), the fastest reliable
		// way to make Client.Run return a terminal error without standing up a full fake WS server.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"nope"}}`))
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	env := &runtimeEnv{server: u, serverSource: "default", token: "tzy_x"}

	var logOut bytes.Buffer
	lg, err := logging.Open(logging.Options{Dest: "stdout", Format: "json"}, &logOut, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lg.Close() }()

	target, _ := url.Parse("http://localhost:3000")
	spec := tunnelSpec{name: "web", target: target, display: target.String(), hostHeader: "preserve"}

	err = runTunnels(context.Background(), env, []tunnelSpec{spec}, ui.New(io.Discard, true, ""), lg, inspectOpts{disabled: true}, io.Discard)
	if err == nil {
		t.Fatal("expected the terminal client error to propagate")
	}
	if !strings.Contains(logOut.String(), `"msg":"tunnel.error"`) {
		t.Fatalf("expected a tunnel.error log line, got: %s", logOut.String())
	}
	if !strings.Contains(logOut.String(), `"name":"web"`) {
		t.Fatalf("expected the tunnel name in the log line, got: %s", logOut.String())
	}
}

// M1 end-to-end: with --log stdout --log-format json, stdout must never carry a plain notice line
// (they must all move to stderr), and every non-empty stdout line that is produced must parse as
// JSON.
func TestHTTPLogStdoutJSONNeverMixedWithPlainNotices(t *testing.T) {
	t.Setenv("TUZY_TOKEN", "tzy_x")
	dir := t.TempDir()
	site := filepath.Join(dir, "site")
	if err := os.Mkdir(site, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errOut)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	root.SetArgs([]string{
		"http", "file://" + filepath.ToSlash(site),
		"--name", "site",
		"--server", "http://localhost:1", // loopback: connect fails/retries, never succeeds
		"--log", "stdout", "--log-format", "json",
		"--no-inspect",
		"--config", filepath.Join(dir, "config.toml"),
	})
	_ = root.ExecuteContext(ctx)

	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("stdout line is not valid JSON: %q (%v)", line, err)
		}
	}
	if strings.Contains(out.String(), "serving files from") {
		t.Fatalf("plain notice leaked into the JSON stdout stream: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "serving files from") {
		t.Fatalf("expected the file-serving notice on stderr, got: %q", errOut.String())
	}
}
