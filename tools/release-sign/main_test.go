package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nhtera/tuzy/internal/update"
)

func TestKeygenSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	err := run([]string{"keygen", key})
	os.Stdout = stdout
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 128)
	n, _ := r.Read(buf)
	pub := strings.TrimSpace(string(buf[:n]))
	if st, _ := os.Stat(key); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode().Perm())
	}
	if err := run([]string{"keygen", key}); err == nil {
		t.Fatal("overwrote an existing key")
	}
	seed, _ := os.ReadFile(key)
	t.Setenv("TUZY_RELEASE_KEY", string(seed))
	sums := filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(sums, []byte("abc  tuzy_1.0.0_linux_amd64.tar.gz\n"), 0o644)
	if err := run([]string{"sign", sums}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(sums)
	sig, _ := os.ReadFile(sums + ".sig")
	if err := update.VerifySignature([]string{pub}, data, sig); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsForeignSignatures(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	if err := run([]string{"keygen", key}); err != nil { // a key that is NOT embedded
		t.Fatal(err)
	}
	seed, _ := os.ReadFile(key)
	t.Setenv("TUZY_RELEASE_KEY", string(seed))
	f := filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := run([]string{"sign", f}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"verify", f}); err == nil {
		t.Fatal("a foreign key's signature verified against the embedded keys")
	}
}
