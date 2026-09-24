// Package update implements `tuzy update`: resolve a GitHub release, verify its signed checksums,
// extract the tuzy binary and replace the running executable atomically.
//
// Trust chain: checksums.txt must carry a valid ed25519 signature (checksums.txt.sig, base64) by
// one of the embedded release keys; the archive must match its sha256 line; the extracted binary
// must report the expected version before it replaces anything.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// DefaultBase is the GitHub releases URL of the CLI.
const DefaultBase = "https://github.com/nhtera/tuzy/releases"

// Size limits for downloads and the extracted binary.
const (
	maxArchive  = 200 << 20
	maxChecksum = 64 << 10
)

// Updater resolves, verifies and installs releases.
type Updater struct {
	Base    string // releases URL (DefaultBase)
	Client  *http.Client
	Current string // running version, e.g. "v1.2.3" or "dev"
	GOOS    string
	GOARCH  string
	Exe     string   // executable to replace (symlinks resolved)
	Keys    []string // base64 ed25519 public keys (ReleaseKeys)
	// VerifyRun runs `<bin> version` (swapped in tests).
	VerifyRun func(bin string) (string, error)
}

// New returns an updater for the running binary.
func New(current string) (*Updater, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return nil, err
	}
	return &Updater{
		Base: DefaultBase, Client: &http.Client{Timeout: 5 * time.Minute}, Current: canonical(current),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Exe: exe, Keys: ReleaseKeys,
		VerifyRun: func(bin string) (string, error) {
			out, err := exec.Command(bin, "version").CombinedOutput()
			return string(out), err
		},
	}, nil
}

func canonical(v string) string {
	if v != "" && v[0] != 'v' {
		v = "v" + v
	}
	return v
}

var releaseTag = regexp.MustCompile(`/releases/download/(v[0-9][^/]*)/`)

// Latest resolves the newest stable release from the FIRST redirect of releases/latest (no API
// rate limit; prereleases are excluded by GitHub). Later hops go to a presigned asset host whose
// URL carries no tag (and tokens that must never be printed), so redirects aren't followed.
func (u *Updater) Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.Base+"/latest/download/checksums.txt", nil)
	if err != nil {
		return "", err
	}
	noFollow := *u.Client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noFollow.Do(req)
	if err != nil {
		return "", fmt.Errorf("check for updates: %w", err)
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("check for updates: no published release found at %s", u.Base)
	case resp.StatusCode < 300 || resp.StatusCode > 399:
		return "", fmt.Errorf("check for updates: %s", resp.Status)
	}
	loc, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
	if err != nil {
		return "", errors.New("check for updates: bad redirect")
	}
	m := releaseTag.FindStringSubmatch(loc.Path)
	if m == nil || !semver.IsValid(m[1]) {
		return "", fmt.Errorf("check for updates: unexpected release path %s", loc.Path) // path only: no query tokens
	}
	return m[1], nil
}

// Newer reports whether version is newer than the running one ("dev" builds never auto-update).
func (u *Updater) Newer(version string) bool {
	return semver.IsValid(u.Current) && semver.Compare(version, u.Current) > 0
}

// AssetName is the archive for this platform, e.g. tuzy_1.2.3_darwin_arm64.tar.gz.
func (u *Updater) AssetName(version string) string {
	ext := ".tar.gz"
	if u.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("tuzy_%s_%s_%s%s", strings.TrimPrefix(version, "v"), u.GOOS, u.GOARCH, ext)
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", path.Base(url), resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", path.Base(url), limit)
	}
	return b, nil
}

// VerifySignature checks sig (base64) over data against any of the keys.
func VerifySignature(keys []string, data, sig []byte) error {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(raw) != ed25519.SignatureSize {
		return errors.New("checksums.txt.sig is malformed")
	}
	for _, k := range keys {
		pub, err := base64.StdEncoding.DecodeString(k)
		if err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, data, raw) {
			return nil
		}
	}
	return errors.New("checksums.txt signature does not match any tuzy release key")
}

func checksumFor(checksums []byte, asset string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not in checksums.txt (unsupported platform?)", asset)
}

// Download fetches and verifies the release archive, returning the extracted binary bytes.
func (u *Updater) Download(ctx context.Context, version string) ([]byte, error) {
	if len(u.Keys) == 0 {
		return nil, errors.New("this build has no release key; update with your package manager or install.sh")
	}
	dir := u.Base + "/download/" + version + "/"
	sums, err := u.get(ctx, dir+"checksums.txt", maxChecksum)
	if err != nil {
		return nil, err
	}
	sig, err := u.get(ctx, dir+"checksums.txt.sig", 1024)
	if err != nil {
		return nil, err
	}
	if err := VerifySignature(u.Keys, sums, sig); err != nil {
		return nil, err
	}
	asset := u.AssetName(version)
	want, err := checksumFor(sums, asset)
	if err != nil {
		return nil, err
	}
	archive, err := u.get(ctx, dir+asset, maxArchive)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s: sha256 mismatch (download corrupted or tampered)", asset)
	}
	return extract(archive, u.GOOS)
}

// extract returns the single tuzy binary from a release archive.
func extract(archive []byte, goos string) ([]byte, error) {
	name := "tuzy"
	if goos == "windows" {
		name = "tuzy.exe"
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if path.Base(f.Name) != name || !f.Mode().IsRegular() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return readCapped(rc)
		}
		return nil, errors.New("tuzy.exe not found in the archive")
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("tuzy not found in the archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			return readCapped(tr)
		}
	}
}

func readCapped(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxArchive+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxArchive {
		return nil, errors.New("binary in the archive is too large")
	}
	return b, nil
}

// Install writes bin next to the executable, checks it reports version, and swaps it in.
func (u *Updater) Install(bin []byte, version string) error {
	dir := filepath.Dir(u.Exe)
	st, err := os.Stat(u.Exe)
	if err != nil {
		return err
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, ".tuzy-update-"+hex.EncodeToString(rnd[:]))
	if u.GOOS == "windows" {
		tmp += ".exe"
	}
	if err := os.WriteFile(tmp, bin, st.Mode().Perm()|0o100); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("no permission to write %s: rerun with sudo, or reinstall with install.sh", dir)
		}
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	out, err := u.VerifyRun(tmp)
	if err != nil || !strings.Contains(out, strings.TrimPrefix(version, "v")) {
		return fmt.Errorf("the downloaded binary doesn't run correctly here (%v): kept the current one", strings.TrimSpace(out))
	}
	return swap(u.GOOS, u.Exe, tmp)
}

// swap replaces exe with tmp. Unix: one atomic rename. Windows: a running exe can be renamed but
// not overwritten, so exe → exe.old-<rand>, tmp → exe (rolled back on failure). Unique .old names:
// a still-running previous binary (e.g. the service) keeps its file locked across updates.
func swap(goos, exe, tmp string) error {
	if goos != "windows" {
		return os.Rename(tmp, exe)
	}
	CleanupOld(exe)
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	old := exe + ".old-" + hex.EncodeToString(rnd[:])
	if err := retry(func() error { return os.Rename(exe, old) }); err != nil {
		return err
	}
	if err := retry(func() error { return os.Rename(tmp, exe) }); err != nil {
		if rerr := retry(func() error { return os.Rename(old, exe) }); rerr != nil {
			return fmt.Errorf("%w; the previous binary is at %s (rename it back to %s)", err, old, exe)
		}
		return err
	}
	return nil
}

// retry: antivirus scanners briefly hold handles on fresh files (Windows).
func retry(fn func() error) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = fn(); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return err
}

// CleanupOld removes leftover <exe>.old-* files from previous Windows updates (best effort: a
// file still in use by a running old process stays until the next run).
func CleanupOld(exe string) {
	olds, _ := filepath.Glob(exe + ".old-*")
	for _, o := range olds {
		_ = os.Remove(o)
	}
}

// Method is how tuzy was installed.
type Method string

// Install methods.
const (
	MethodBinary Method = "binary" // install.sh / manual download: self-update allowed
	MethodBrew   Method = "brew"
	MethodScoop  Method = "scoop"
	MethodGo     Method = "go"
)

// DetectMethod classifies the (symlink-resolved) executable path.
func DetectMethod(exe, gopath, gobin, home string) Method {
	p := filepath.ToSlash(exe)
	switch {
	case strings.Contains(p, "/Cellar/"), strings.Contains(p, "/Caskroom/"): // brew formula / cask payloads
		return MethodBrew
	case strings.Contains(strings.ToLower(p), "/scoop/apps/"):
		return MethodScoop
	}
	dirs := []string{gobin}
	for _, gp := range filepath.SplitList(gopath) {
		dirs = append(dirs, filepath.Join(gp, "bin"))
	}
	if gopath == "" && home != "" {
		dirs = append(dirs, filepath.Join(home, "go", "bin"))
	}
	for _, d := range dirs {
		if d != "" && filepath.Clean(filepath.Dir(exe)) == filepath.Clean(d) {
			return MethodGo
		}
	}
	return MethodBinary
}

// Hint is the command a package-managed install should run instead of self-updating.
func (m Method) Hint() string {
	switch m {
	case MethodBrew:
		return "brew upgrade tuzy"
	case MethodScoop:
		return "scoop update tuzy"
	case MethodGo:
		return "go install github.com/nhtera/tuzy/cmd/tuzy@latest"
	}
	return ""
}
