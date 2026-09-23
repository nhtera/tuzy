// Package auth stores API tokens: the OS keychain (service "tuzy", account = server host), with a
// 0600 file fallback ($UserConfigDir/tuzy/credentials.json) where no keychain is available.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
)

const keyringService = "tuzy"

// ErrNoToken means the user is not logged in to that server.
var ErrNoToken = errors.New("not logged in: run `tuzy login`")

// Store reads and writes tokens per server host.
type Store struct {
	// FilePath is the fallback credentials file.
	FilePath string
	// Notice is called once when the file fallback is used (e.g. to print a warning).
	Notice func(string)
}

// DefaultStore uses $UserConfigDir/tuzy/credentials.json as the fallback.
func DefaultStore() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return &Store{FilePath: filepath.Join(dir, "tuzy", "credentials.json")}, nil
}

// Get returns the stored token for host (keychain first, then the file). A keychain failure other
// than "not found" is reported when the file has no token either (not disguised as logged out).
func (s *Store) Get(host string) (string, error) {
	t, kerr := keyring.Get(keyringService, host)
	if kerr == nil && t != "" {
		return t, nil
	}
	creds, err := s.readFile()
	if err != nil {
		return "", err
	}
	if t := creds[host]; t != "" {
		return t, nil
	}
	if kerr != nil && !errors.Is(kerr, keyring.ErrNotFound) {
		return "", fmt.Errorf("%w (OS keychain unavailable: %v)", ErrNoToken, kerr)
	}
	return "", ErrNoToken
}

// Set stores the token for host, preferring the keychain.
func (s *Store) Set(host, token string) error {
	if err := keyring.Set(keyringService, host, token); err == nil {
		_ = s.deleteFromFile(host) // don't leave a stale plaintext copy behind
		return nil
	} else if s.Notice != nil {
		s.Notice(fmt.Sprintf("no OS keychain available (%v); storing the token in %s (mode 0600)", err, s.FilePath))
	}
	creds, err := s.readFile()
	if err != nil {
		return err
	}
	creds[host] = token
	return s.writeFile(creds)
}

// Delete removes the token for host from both places.
func (s *Store) Delete(host string) error {
	if err := s.deleteFromFile(host); err != nil {
		return err
	}
	if err := keyring.Delete(keyringService, host); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		// The keychain may be unavailable (file-fallback users): only fail if it still holds a token.
		if _, gerr := keyring.Get(keyringService, host); gerr == nil {
			return fmt.Errorf("could not remove the token from the OS keychain: %w", err)
		}
	}
	return nil
}

func (s *Store) readFile() (map[string]string, error) {
	creds := map[string]string{}
	b, err := os.ReadFile(s.FilePath)
	if errors.Is(err, fs.ErrNotExist) {
		return creds, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", s.FilePath, err)
	}
	for k, v := range f.Tokens {
		creds[k] = v
	}
	return creds, nil
}

func (s *Store) writeFile(creds map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.FilePath), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(map[string]any{"tokens": creds}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.FilePath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil { // umask-proof
		return err
	}
	return os.Rename(tmp, s.FilePath)
}

func (s *Store) deleteFromFile(host string) error {
	creds, err := s.readFile()
	if err != nil {
		return err
	}
	if _, ok := creds[host]; !ok {
		return nil
	}
	delete(creds, host)
	return s.writeFile(creds)
}
