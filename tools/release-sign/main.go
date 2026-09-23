// Command release-sign manages the ed25519 key that signs release checksums (`tuzy update`
// verifies checksums.txt.sig against the keys embedded in internal/update/keys.go).
//
//	release-sign keygen <private-key-file>   write a new key (0600), print the public key (base64)
//	release-sign sign <file>                  write <file>.sig using TUZY_RELEASE_KEY (base64 seed)
//
// In CI the key comes from a secret (TUZY_RELEASE_KEY); it is never committed.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: release-sign keygen <private-key-file> | sign <file>")
	}
	switch args[0] {
	case "keygen":
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(args[1], os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // never overwrite a key
		if err != nil {
			return fmt.Errorf("refusing to write the release key: %w", err)
		}
		if _, err := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n"); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Println(base64.StdEncoding.EncodeToString(pub))
		return nil
	case "sign":
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("TUZY_RELEASE_KEY")))
		if err != nil || len(seed) != ed25519.SeedSize {
			return errors.New("TUZY_RELEASE_KEY must hold the base64 key seed")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), data)
		return os.WriteFile(args[1]+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
	}
	return fmt.Errorf("unknown command %q", args[0])
}
