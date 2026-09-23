package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zalando/go-keyring"

	"github.com/nhtera/tuzy/internal/auth"
)

// run executes the CLI with args and stdin, isolated from the real user config and keychain.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	keyring.MockInit()
	dir := t.TempDir()
	store := &auth.Store{FilePath: filepath.Join(dir, "credentials.json")}
	prev := tokenStore
	tokenStore = func(*cobra.Command) (*auth.Store, error) { return store, nil }
	t.Cleanup(func() { tokenStore = prev })
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append(args, "--config", filepath.Join(dir, "config.toml")))
	err := root.Execute()
	return out.String(), err
}

func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/api/v1/auth/email/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"login_id": "lid", "expires_in": 600})
		case "/api/v1/auth/email/verify":
			if in["code"] != "123456" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_code","message":"wrong"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tzy_fromlogin", "user": map[string]string{"id": "usr_1", "email": in["login_id"] + "@x"}})
		case "/api/v1/me":
			if r.Header.Get("Authorization") != "Bearer tzy_fromlogin" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"code":"unauthorized","message":"not logged in: run ` + "`tuzy login`" + `"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"user": map[string]string{"id": "usr_1", "email": "a@b.c"}, "token": map[string]string{"id": "tok_1", "scope": "full", "label": "mbp"}})
		case "/api/v1/tokens/current":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginRetriesWrongCodeThenStoresToken(t *testing.T) {
	srv := fakeAPI(t)
	t.Setenv("TUZY_TOKEN", "")
	out, err := run(t, "000000\n123 456\n", "login", "--email", "a@b.c", "--server", srv.URL)
	if err != nil {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if !strings.Contains(out, "didn't work") || !strings.Contains(out, "✓ Logged in") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestLoginWhoamiLogout(t *testing.T) {
	srv := fakeAPI(t)
	t.Setenv("TUZY_TOKEN", "")
	keyring.MockInit()
	dir := t.TempDir()
	store := &auth.Store{FilePath: filepath.Join(dir, "c.json")}
	prev := tokenStore
	tokenStore = func(*cobra.Command) (*auth.Store, error) { return store, nil }
	defer func() { tokenStore = prev }()
	exec := func(stdin string, args ...string) string {
		var out bytes.Buffer
		root := NewRootCmd()
		root.SetIn(strings.NewReader(stdin))
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(append(args, "--server", srv.URL, "--config", filepath.Join(dir, "none.toml")))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	exec("123456\n", "login", "--email", "a@b.c")
	if out := exec("", "whoami"); !strings.Contains(out, "a@b.c") {
		t.Fatalf("whoami: %s", out)
	}
	exec("", "logout")
	u, _ := url.Parse(srv.URL)
	if _, err := store.Get(u.Host); err == nil {
		t.Fatal("token still stored after logout")
	}
}

func TestHTTPNeedsLogin(t *testing.T) {
	t.Setenv("TUZY_TOKEN", "")
	_, err := run(t, "", "http", "3000", "--name", "shop", "--server", "http://localhost:1")
	if err == nil || !strings.Contains(err.Error(), "tuzy login") {
		t.Fatalf("err = %v", err)
	}
}

func TestTuzyTokenDestinationRule(t *testing.T) {
	t.Setenv("TUZY_TOKEN", "tzy_ci")
	t.Setenv("TUZY_SERVER", "https://evil.example")
	if _, err := run(t, "", "whoami"); err == nil || !strings.Contains(err.Error(), "refusing to send TUZY_TOKEN") {
		t.Fatalf("expected refusal, got %v", err)
	}
	t.Setenv("TUZY_SERVER", "http://localhost:1") // loopback dev server is fine (fails later on connect)
	if _, err := run(t, "", "whoami"); err != nil && strings.Contains(err.Error(), "refusing") {
		t.Fatalf("loopback refused: %v", err)
	}
}

func TestParseServer(t *testing.T) {
	for in, want := range map[string]string{
		"https://tuzy.dev/":      "https://tuzy.dev",
		"http://localhost:8787":  "http://localhost:8787",
		"http://127.0.0.1:8787":  "http://127.0.0.1:8787",
		"http://shop.localhost/": "http://shop.localhost",
	} {
		u, err := parseServer(in)
		if err != nil || u.String() != want {
			t.Errorf("parseServer(%q) = %v, %v", in, u, err)
		}
	}
	for _, bad := range []string{"http://tuzy.dev", "ftp://x", "tuzy.dev", ""} {
		if _, err := parseServer(bad); err == nil {
			t.Errorf("parseServer(%q) should fail", bad)
		}
	}
}

func TestLogoutKeepsTokenWhenRevokeFailsUnlessLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"down"}}`))
	}))
	defer srv.Close()
	t.Setenv("TUZY_TOKEN", "")
	keyring.MockInit()
	dir := t.TempDir()
	store := &auth.Store{FilePath: filepath.Join(dir, "c.json")}
	prev := tokenStore
	tokenStore = func(*cobra.Command) (*auth.Store, error) { return store, nil }
	defer func() { tokenStore = prev }()
	u, _ := url.Parse(srv.URL)
	_ = store.Set(u.Host, "tzy_keep")
	exec := func(args ...string) error {
		root := NewRootCmd()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs(append(args, "--server", srv.URL, "--config", filepath.Join(dir, "none.toml")))
		return root.Execute()
	}
	if err := exec("logout"); err == nil {
		t.Fatal("logout should fail when the revoke fails")
	}
	if tok, _ := store.Get(u.Host); tok != "tzy_keep" {
		t.Fatal("token must be kept when the revoke failed")
	}
	if err := exec("logout", "--local"); err != nil {
		t.Fatalf("logout --local: %v", err)
	}
	if _, err := store.Get(u.Host); err == nil {
		t.Fatal("token still stored after logout --local")
	}
}

func TestLoginAllowedWhenTokenRefusedForServer(t *testing.T) {
	srv := fakeAPI(t)
	t.Setenv("TUZY_TOKEN", "tzy_ci")
	t.Setenv("TUZY_SERVER", "https://evil.example") // refused for token use, but login must still work with --server
	if _, err := run(t, "123456\n", "login", "--email", "a@b.c", "--server", srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
}
