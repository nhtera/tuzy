package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nhtera/tuzy/internal/ui"
)

func TestConfigCheck(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "public"), 0o700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "tuzy.toml")
	_ = os.WriteFile(good, []byte("[tunnels.web]\naddr = \"3000\"\n[tunnels.site]\naddr = \"file://public\"\n"), 0o600)
	out, err := run(t, "", "config", "check", "--file", good)
	if err != nil || !strings.Contains(out, "2 tunnel(s) site, web") {
		t.Fatalf("%v\n%s", err, out)
	}

	bad := filepath.Join(dir, "bad.toml")
	_ = os.WriteFile(bad, []byte("[tunnels.leak]\naddr = \"file:///etc\"\n[tunnels.escape]\naddr = \"file://../..\"\n[tunnels.ftp]\naddr = \"ftp://x\"\n"), 0o600)
	out, err = run(t, "", "config", "check", "--file", bad)
	if err == nil || strings.Count(out, ui.Cross) != 3 {
		t.Fatalf("want 3 problems, got %v\n%s", err, out)
	}

	server := filepath.Join(dir, "server.toml")
	_ = os.WriteFile(server, []byte("server = \"https://evil.example\"\n"), 0o600)
	if out, err := run(t, "", "config", "check", "--file", server); err == nil || !strings.Contains(out, `"server" is not allowed`) {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestConfigAddTokenValidatesFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tzy_good" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"not logged in"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","email":"ci@x.y"},"token":{"id":"tok_1","scope":"connect","label":"ci"}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TUZY_TOKEN", "")
	t.Setenv("TUZY_SERVER", "https://evil.example") // env-chosen, non-local server: refused
	if out, err := run(t, "tzy_good\n", "config", "add-token", "-"); err == nil || !strings.Contains(err.Error(), "refusing to send the token") {
		t.Fatalf("token sent to an env-chosen server: %v\n%s", err, out)
	}
	t.Setenv("TUZY_SERVER", srv.URL) // loopback test server: allowed
	if out, err := run(t, "", "config", "add-token", "tzy_bad"); err == nil {
		t.Fatalf("bad token stored:\n%s", out)
	}
	out, err := run(t, "tzy_good\n", "config", "add-token", "-", "--file")
	if err != nil || !strings.Contains(out, "stored a connect-scope token for ci@x.y") {
		t.Fatalf("%v\n%s", err, out)
	}
}
