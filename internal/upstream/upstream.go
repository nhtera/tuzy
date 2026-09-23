// Package upstream parses the local target an agent forwards to ("upstream" in ngrok's vocabulary):
// a bare port, host:port, http://, https:// or file:// address. It also builds the per-target
// pieces the https and file:// kinds need (a TLS-configured transport, and safe directory
// resolution for file://), so internal/cli can wire them into a tunnel without either side
// depending on the other.
//
// This package intentionally does not import internal/tunnel: internal/tunnel's own test files
// (package tunnel) exercise https/file targets built with this package, and internal/tunnel
// importing upstream (for its LocalTransport plumbing) would create an import cycle if upstream
// imported back. The tiny bits shared with tunnel.NewLocalTransport are duplicated below instead.
package upstream

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Kind is what an addr resolves to.
type Kind int

// Target kinds.
const (
	KindHTTP Kind = iota
	KindHTTPS
	KindFile
)

// String renders the kind as used in error messages and logs.
func (k Kind) String() string {
	switch k {
	case KindHTTP:
		return "http"
	case KindHTTPS:
		return "https"
	case KindFile:
		return "file"
	default:
		return "unknown"
	}
}

// Target is one parsed upstream addr.
type Target struct {
	Kind Kind
	URL  *url.URL // http, https: e.g. http://localhost:3000
	Dir  string   // file: the raw path exactly as written in the addr — see ResolveCLIDir / ResolveProjectDir
}

// Parse turns a user target into a Target:
//
//	3000                   → http, http://localhost:3000
//	127.0.0.1:8080         → http, http://127.0.0.1:8080
//	http://host:port       → http, as-is
//	https://host:port      → https, as-is
//	file:///abs/dir        → file, Dir "/abs/dir"
//	file://relative/dir    → file, Dir "relative/dir" (url.Parse reads "relative" as the authority)
func Parse(s string) (Target, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Target{}, fmt.Errorf("missing target: use a port (3000), host:port, http://, https:// or file:// URL")
	}
	if !strings.Contains(trimmed, "://") {
		u, err := parseBare(trimmed)
		if err != nil {
			return Target{}, err
		}
		return Target{Kind: KindHTTP, URL: u}, nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return Target{}, fmt.Errorf("invalid target %q: %w", s, err)
	}
	switch u.Scheme {
	case "http", "https":
		if u.Host == "" {
			return Target{}, fmt.Errorf("invalid target %q: missing host", s)
		}
		u.RawQuery, u.Fragment = "", ""
		kind := KindHTTP
		if u.Scheme == "https" {
			kind = KindHTTPS
		}
		return Target{Kind: kind, URL: u}, nil
	case "file":
		dir := u.Path
		if u.Host != "" {
			// "file://relative/dir": url.Parse reads the first path segment as the authority
			// (host); rebuild the intended relative path from it.
			dir = u.Host + u.Path
		}
		if dir == "" {
			return Target{}, fmt.Errorf("invalid target %q: empty file path", s)
		}
		dir = stripWindowsDriveSlash(dir, runtime.GOOS)
		return Target{Kind: KindFile, Dir: dir}, nil
	default:
		return Target{}, fmt.Errorf("unsupported target scheme %q", u.Scheme)
	}
}

// windowsDriveSlash matches the leading "/" url.Parse leaves in front of a Windows drive letter:
// file:///C:/site parses to a Path of "/C:/site" (the URL scheme always uses a leading "/" before
// an absolute path, drive letter or not), which filepath.Abs/EvalSymlinks on Windows treats as a
// bogus path rooted at "\C:\site" instead of "C:\site".
var windowsDriveSlash = regexp.MustCompile(`^/[A-Za-z]:(/|$)`)

// stripWindowsDriveSlash removes that leading "/" when goos is "windows". Takes goos as a parameter
// (instead of reading runtime.GOOS itself) purely so the mapping is unit-testable for both OSes from
// a single (non-Windows) CI host.
func stripWindowsDriveSlash(dir, goos string) string {
	if goos == "windows" && windowsDriveSlash.MatchString(dir) {
		return dir[1:]
	}
	return dir
}

// parseBare handles a bare port or host:port (no "://"), mirroring the equivalent bare-target
// handling that used to live in internal/tunnel (removed: dead code outside its own tests, this is
// the only version reachable from production code).
func parseBare(s string) (*url.URL, error) {
	if port, err := strconv.Atoi(s); err == nil {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port %d", port)
		}
		return &url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", s)}, nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return nil, fmt.Errorf("invalid target %q: use a port (3000), host:port or http://host:port", s)
	}
	return &url.URL{Scheme: "http", Host: s}, nil
}

// TLSOptions configures an https upstream's transport.
type TLSOptions struct {
	// SNI, if set, is both the ServerName sent on the handshake and the name verified against the
	// upstream's certificate.
	SNI string
	// Insecure disables certificate verification entirely. Callers must print a warning: this is a
	// deliberate weakening of transport security, only for a known self-signed local app.
	Insecure bool
}

// NewHTTPSTransport returns a per-target clone of the default local transport for an https
// upstream. ForceAttemptHTTP2 is left unset so the upstream stays HTTP/1.1, matching an http
// target and keeping the relay's framing simple.
func NewHTTPSTransport(opts TLSOptions) *http.Transport {
	t := baseTransport()
	t.TLSClientConfig = &tls.Config{
		ServerName:         opts.SNI,
		InsecureSkipVerify: opts.Insecure, //nolint:gosec // explicit opt-in via --upstream-insecure / upstream_insecure, warned at start
		MinVersion:         tls.VersionTLS12,
	}
	return t
}

// baseTransport mirrors tunnel.NewLocalTransport()'s defaults: no proxy (the target is on the same
// machine), no compression (bytes relayed to the visitor must match what the local app sent), a
// small idle-connection pool.
func baseTransport() *http.Transport {
	return &http.Transport{
		Proxy:               nil,
		DisableCompression:  true,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
}

// ResolveCLIDir resolves a file:// target's directory given directly on the command line (e.g.
// `tuzy http file:///abs/dir` or a relative file:// addr): relative paths resolve against the
// current working directory, absolute paths are used as-is. Symlinks are fully resolved so the
// fileserver always opens the real directory. Unlike ResolveProjectDir, escaping the working
// directory is allowed here: the user typed this on their own command line.
func ResolveCLIDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("file target %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("file target %q: %w", dir, err)
	}
	return checkDir(dir, resolved)
}

// ResolveProjectDir resolves a file:// target declared in tuzy.toml. SECURITY: a project file
// (which may come from a cloned, untrusted repo) must never be able to point outside its own
// directory, so dir must be a relative path that — after filepath.Abs and filepath.EvalSymlinks —
// still resolves inside projectDir (the directory containing tuzy.toml, itself resolved the same
// way). Absolute file:// paths and any ".." escape (direct or via a symlink) are rejected.
func ResolveProjectDir(projectDir, dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return "", fmt.Errorf("file target %q must be a relative path inside the project directory (absolute file:// paths are only allowed on the command line)", dir)
	}
	base, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	baseReal, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("project directory %q: %w", projectDir, err)
	}
	absJoined, err := filepath.Abs(filepath.Join(base, dir))
	if err != nil {
		return "", err
	}
	if !withinDir(base, absJoined) {
		return "", fmt.Errorf("file target %q escapes the project directory", dir)
	}
	resolved, err := filepath.EvalSymlinks(absJoined)
	if err != nil {
		return "", fmt.Errorf("file target %q: %w", dir, err)
	}
	if !withinDir(baseReal, resolved) {
		return "", fmt.Errorf("file target %q escapes the project directory", dir)
	}
	return checkDir(dir, resolved)
}

// checkDir confirms resolved (already absolute and symlink-resolved) is a directory.
func checkDir(dir, resolved string) (string, error) {
	fi, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("file target %q: %w", dir, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("file target %q is not a directory", dir)
	}
	return resolved, nil
}

// withinDir reports whether target (both absolute) is base or inside it.
func withinDir(base, target string) bool {
	if base == target {
		return true
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
