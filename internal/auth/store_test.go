package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"
)

func TestKeychainRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := &Store{FilePath: filepath.Join(t.TempDir(), "credentials.json")}
	if _, err := s.Get("tuzy.dev"); !errors.Is(err, ErrNoToken) {
		t.Fatalf("empty get: %v", err)
	}
	if err := s.Set("tuzy.dev", "tzy_a"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get("tuzy.dev"); got != "tzy_a" {
		t.Fatalf("got %q", got)
	}
	if _, err := os.Stat(s.FilePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("keychain path must not write the plaintext file")
	}
	if err := s.Delete("tuzy.dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("tuzy.dev"); !errors.Is(err, ErrNoToken) {
		t.Fatal("token still present after delete")
	}
}

func TestFileFallbackIsPrivate(t *testing.T) {
	keyring.MockInitWithError(errors.New("no dbus"))
	var notice string
	s := &Store{FilePath: filepath.Join(t.TempDir(), "tuzy", "credentials.json"), Notice: func(m string) { notice = m }}
	if err := s.Set("localhost:8787", "tzy_b"); err != nil {
		t.Fatal(err)
	}
	if notice == "" {
		t.Fatal("expected a fallback notice")
	}
	if got, _ := s.Get("localhost:8787"); got != "tzy_b" {
		t.Fatalf("got %q", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(s.FilePath)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v err %v", st.Mode().Perm(), err)
		}
	}
	if err := s.Delete("localhost:8787"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("localhost:8787"); !errors.Is(err, ErrNoToken) {
		t.Fatal("token still present after delete")
	}
}
