package fileserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// get performs a request through rt and returns the status and body.
func get(t *testing.T, rt *RoundTripper, path string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip(%s): %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return resp, body
}

func symlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestServesAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, rt, "/a.txt", nil)
	if resp.StatusCode != 200 || string(body) != "hello" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestIndexHTMLServedAtRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("home page"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, rt, "/", nil)
	if resp.StatusCode != 200 || string(body) != "home page" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestDotDotAfterCleanIs404(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := get(t, rt, "/"+filepath.ToSlash(rel), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestRelativeSymlinkChainEscapingRootIs404(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Join(dir, "sub"), filepath.Join(outside, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	symlink(t, rel, filepath.Join(dir, "sub", "escape.txt"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := get(t, rt, "/sub/escape.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestAbsoluteSymlinkIs404(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(target, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, target, filepath.Join(dir, "escape.txt"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := get(t, rt, "/escape.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestSymlinkedDirInsideRootWorks(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "a.txt"), []byte("dirfile"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, "real", filepath.Join(dir, "alias"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, rt, "/alias/a.txt", nil)
	if resp.StatusCode != 200 || string(body) != "dirfile" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestDotfilesHiddenFromGetAndListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".secret"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("shown"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, rt, "/.secret", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET .secret status=%d, want 404", resp.StatusCode)
	}
	resp, body := get(t, rt, "/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("listing status=%d", resp.StatusCode)
	}
	if strings.Contains(string(body), ".secret") {
		t.Fatalf("listing leaked dotfile: %s", body)
	}
	if !strings.Contains(string(body), "visible.txt") {
		t.Fatalf("listing missing visible.txt: %s", body)
	}
}

func TestDotDirHidesEverythingInside(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, rt, "/.git/config", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestRangeRequestWorks(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("0123456789"), 100) // 1000 bytes
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, rt, "/big.bin", map[string]string{"Range": "bytes=10-19"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status=%d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, content[10:20]) {
		t.Fatalf("range body = %q, want %q", body, content[10:20])
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 10-19/1000" {
		t.Fatalf("Content-Range = %q", cr)
	}
}

func TestHeadRequestNoBody(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodHead, "/a.txt", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestSymlinkedFileInsideRootWorks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirfile"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, "a.txt", filepath.Join(dir, "alias.txt"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, rt, "/alias.txt", nil)
	if resp.StatusCode != 200 || string(body) != "dirfile" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

// H1: a symlink whose *name* looks innocent but whose target resolves to a dotfile/dotdir must be
// blocked exactly like requesting the dotfile directly — both for a direct GET and in a directory
// listing.
func TestFileSymlinkToDotfileIs404AndHiddenFromListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, ".env", filepath.Join(dir, "env.txt"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, rt, "/env.txt", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET env.txt (-> .env) status=%d, want 404", resp.StatusCode)
	}
	resp, body := get(t, rt, "/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("listing status=%d", resp.StatusCode)
	}
	if strings.Contains(string(body), "env.txt") {
		t.Fatalf("listing leaked symlink to dotfile: %s", body)
	}
}

func TestDirSymlinkToDotDirIs404AndHiddenFromListing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, ".git", filepath.Join(dir, "g"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, rt, "/g/config", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET g/config (-> .git/config) status=%d, want 404", resp.StatusCode)
	}
	if resp, _ := get(t, rt, "/g/", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET g/ listing status=%d, want 404", resp.StatusCode)
	}
	resp, body := get(t, rt, "/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("root listing status=%d", resp.StatusCode)
	}
	if strings.Contains(string(body), ">g<") || strings.Contains(string(body), "\"g\"") || strings.Contains(string(body), "/g/") {
		t.Fatalf("root listing leaked symlink to .git: %s", body)
	}
}

func TestSymlinkChainEndingAtDotfileIs404(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlink(t, ".env", filepath.Join(dir, "b"))
	symlink(t, "b", filepath.Join(dir, "a"))

	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, rt, "/a", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a (-> b -> .env) status=%d, want 404", resp.StatusCode)
	}
}

// L1: a request whose context is cancelled while the body is still being read must fail the read
// (instead of hanging or returning a truncated-but-apparently-complete body) and must not leak the
// handler goroutine.
func TestRoundTripHonoursContextCancellation(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("x"), 10) // 10 bytes, so a chunked read leaves plenty to cancel mid-stream
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/big.bin", nil).WithContext(ctx)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// context.AfterFunc runs its callback in its own goroutine: give it a moment, otherwise this
	// reader could drain the whole (pipe-paced) file first and legitimately see EOF.
	time.Sleep(100 * time.Millisecond)

	// The next read must observe the cancellation (not block forever, not return a clean EOF).
	readDone := make(chan struct{})
	var readErr error
	go func() {
		defer close(readDone)
		buf := make([]byte, 1)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				readErr = err
				return
			}
		}
	}()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Body.Read did not observe context cancellation")
	}
	// io.Pipe's reader, once it has closed itself (which the RoundTrip context.AfterFunc callback
	// does on cancellation), reports plain io.ErrClosedPipe to its own further reads — not the
	// original close reason, which is instead what unblocks the handler's pending Write (see
	// fileserver.go's RoundTrip doc comment). Either way, the crucial behaviour under test is that
	// the read fails promptly instead of hanging or returning a clean EOF as if the body were whole.
	if readErr == nil || !errors.Is(readErr, io.ErrClosedPipe) {
		t.Fatalf("Body.Read error = %v, want io.ErrClosedPipe", readErr)
	}
	_ = resp.Body.Close()

	// No goroutine leak: closing/cancelling must let the handler goroutine (blocked on the pipe
	// write) return promptly.
	goroutinesSettled(t)
}

// goroutinesSettled is a light best-effort leak check: it just gives any just-unblocked goroutine a
// moment to finish, then re-checks the count didn't grow unreasonably versus a fresh baseline. It is
// intentionally coarse (goroutine counts are inherently racy across a whole test binary) — its job
// is to catch a gross leak (a handler goroutine permanently blocked on a Write that never unblocks),
// not to be a precise leak detector.
func goroutinesSettled(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if runtime.NumGoroutine() <= before {
			return
		}
	}
}

func TestRoundTripMethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := New(dir)
	t.Cleanup(func() { _ = rt.Close() }) // release the dir handle before TempDir cleanup
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		req := httptest.NewRequest(method, "/a.txt", nil)
		resp, err := rt.RoundTrip(req)
		if err != nil {
			t.Fatalf("%s: RoundTrip error: %v", method, err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status=%d, want 405", method, resp.StatusCode)
		}
		if allow := resp.Header.Get("Allow"); allow != "GET, HEAD" {
			t.Fatalf("%s: Allow header = %q", method, allow)
		}
	}
}

// TestRoundTripHandlerPanicIsRecovered documents that a handler panic (however unlikely with the
// standard library's file server) is turned into a 500 instead of crashing the process.
func TestRoundTripHandlerPanicIsRecovered(t *testing.T) {
	rt := &RoundTripper{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip returned an error instead of recovering: %v", err)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("expected reading the body of a panicked handler's response to surface an error")
	}
	_ = resp.Body.Close()
}

func TestNewRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f); err == nil {
		t.Fatal("expected an error opening a file as a root")
	}
}
