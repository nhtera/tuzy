package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseEveryForm(t *testing.T) {
	cases := []struct {
		in      string
		kind    Kind
		wantURL string
		wantDir string
	}{
		{"3000", KindHTTP, "http://localhost:3000", ""},
		{"127.0.0.1:8080", KindHTTP, "http://127.0.0.1:8080", ""},
		{"localhost:5173", KindHTTP, "http://localhost:5173", ""},
		{"http://localhost:5173", KindHTTP, "http://localhost:5173", ""},
		{"http://localhost:5173/app/", KindHTTP, "http://localhost:5173/app/", ""},
		{"[::1]:9000", KindHTTP, "http://[::1]:9000", ""},
		{"https://localhost:8443", KindHTTPS, "https://localhost:8443", ""},
		{"https://api.local:8443/v1?x=1#f", KindHTTPS, "https://api.local:8443/v1", ""},
		{"file:///abs/dir", KindFile, "", "/abs/dir"},
		{"file://relative/dir", KindFile, "", "relative/dir"},
		{"file://./site", KindFile, "", "./site"},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if got.Kind != c.kind {
			t.Errorf("Parse(%q).Kind = %v, want %v", c.in, got.Kind, c.kind)
		}
		if c.wantURL != "" && (got.URL == nil || got.URL.String() != c.wantURL) {
			t.Errorf("Parse(%q).URL = %v, want %s", c.in, got.URL, c.wantURL)
		}
		if got.Dir != c.wantDir {
			t.Errorf("Parse(%q).Dir = %q, want %q", c.in, got.Dir, c.wantDir)
		}
	}
	for _, bad := range []string{"", "0", "70000", "localhost", "https://", "ftp://x", "http://", "file://"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

// L4: on Windows, file:///C:/site must resolve to Dir "C:/site", not the bogus "/C:/site" (which
// filepath treats as rooted under the current drive's "\", not C:\). Exercised through a goos
// parameter so both OSes are covered from a single (e.g. non-Windows) CI host.
func TestStripWindowsDriveSlash(t *testing.T) {
	cases := []struct{ dir, goos, want string }{
		{"/C:/site", "windows", "C:/site"},
		{"/c:/site", "windows", "c:/site"},
		{"/C:", "windows", "C:"},
		{"/abs/dir", "windows", "/abs/dir"}, // no drive letter: untouched
		{"relative/dir", "windows", "relative/dir"},
		{"/C:/site", "linux", "/C:/site"}, // non-Windows: never touched
		{"/C:/site", "darwin", "/C:/site"},
	}
	for _, c := range cases {
		if got := stripWindowsDriveSlash(c.dir, c.goos); got != c.want {
			t.Errorf("stripWindowsDriveSlash(%q, %q) = %q, want %q", c.dir, c.goos, got, c.want)
		}
	}
}

func TestResolveCLIDir(t *testing.T) {
	dir := t.TempDir()
	resolved, err := ResolveCLIDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != resolvedDir {
		t.Fatalf("resolved = %q, want %q", resolved, resolvedDir)
	}
	if _, err := ResolveCLIDir(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCLIDir(f); err == nil {
		t.Fatal("expected an error for a file, not a directory")
	}
}

func TestResolveProjectDir(t *testing.T) {
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, "site"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveProjectDir(proj, "site"); err != nil {
		t.Fatalf("relative inside: %v", err)
	}
	if _, err := ResolveProjectDir(proj, "./site"); err != nil {
		t.Fatalf("relative inside (./): %v", err)
	}

	outside := t.TempDir()
	if _, err := ResolveProjectDir(proj, "../"+filepath.Base(outside)); err == nil {
		t.Fatal("expected a .. escape to be rejected")
	}
	if _, err := ResolveProjectDir(proj, outside); err == nil {
		t.Fatal("expected an absolute path to be rejected")
	}
	if err := os.Symlink(outside, filepath.Join(proj, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := ResolveProjectDir(proj, "escape"); err == nil {
		t.Fatal("expected a symlink escape to be rejected")
	}
}

func TestNewHTTPSTransportInsecureToggle(t *testing.T) {
	app := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer app.Close()

	strict := &http.Client{Transport: NewHTTPSTransport(TLSOptions{})}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, app.URL, nil)
	if _, err := strict.Do(req); err == nil {
		t.Fatal("expected a certificate error without --upstream-insecure")
	}

	insecure := &http.Client{Transport: NewHTTPSTransport(TLSOptions{Insecure: true})}
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, app.URL, nil)
	resp, err := insecure.Do(req2)
	if err != nil {
		t.Fatalf("insecure request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}
