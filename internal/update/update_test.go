package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type release struct {
	version  string
	archive  []byte
	sums     []byte
	sig      []byte
	priv     ed25519.PrivateKey
	pub      string
	asset    string
	requests []string
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		n string
		b []byte
	}{{"README.md", []byte("hi")}, {name, body}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.n, Mode: 0o755, Size: int64(len(f.b)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(f.b)
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func newRelease(t *testing.T, version, goos string) *release {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := &release{version: version, priv: priv, pub: base64.StdEncoding.EncodeToString(pub)}
	u := &Updater{GOOS: goos, GOARCH: "arm64"}
	r.asset = u.AssetName(version)
	bin := []byte("NEW BINARY " + version)
	if goos == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("tuzy.exe")
		_, _ = w.Write(bin)
		_ = zw.Close()
		r.archive = buf.Bytes()
	} else {
		r.archive = tarGz(t, "tuzy", bin)
	}
	sum := sha256.Sum256(r.archive)
	r.sums = []byte(fmt.Sprintf("%s  %s\n%s  other_asset.tar.gz\n", hex.EncodeToString(sum[:]), r.asset, strings.Repeat("0", 64)))
	r.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(priv, r.sums)))
	return r
}

func (r *release) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.requests = append(r.requests, req.URL.Path)
		dir := "/releases/download/" + r.version + "/"
		switch req.URL.Path {
		case "/releases/latest/download/checksums.txt":
			http.Redirect(w, req, dir+"checksums.txt", http.StatusFound)
		case dir + "checksums.txt-asset-host":
			w.WriteHeader(http.StatusForbidden) // the presigned asset host (never reached by Latest)
		case dir + "checksums.txt":
			_, _ = w.Write(r.sums)
		case dir + "checksums.txt.sig":
			_, _ = w.Write(r.sig)
		case dir + r.asset:
			_, _ = w.Write(r.archive)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func updater(t *testing.T, r *release, srv *httptest.Server, goos string) *Updater {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "tuzy")
	if err := os.WriteFile(exe, []byte("OLD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Updater{
		Base: srv.URL + "/releases", Client: srv.Client(), Current: "v1.0.0", GOOS: goos, GOARCH: "arm64",
		Exe: exe, Keys: []string{r.pub},
		VerifyRun: func(bin string) (string, error) {
			b, err := os.ReadFile(bin)
			return "tuzy " + strings.TrimPrefix(strings.TrimPrefix(string(b), "NEW BINARY "), "v") + " darwin/arm64", err
		},
	}
}

func TestLatestReadsTheFirstRedirectOnly(t *testing.T) {
	// Real GitHub: releases/latest → /releases/download/vX/… → presigned asset host (no tag).
	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(asset.Close)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/releases/latest/download/checksums.txt":
			http.Redirect(w, req, "/releases/download/v1.4.2/checksums.txt", http.StatusFound)
		default:
			http.Redirect(w, req, asset.URL+"/github-production-release-asset/1/x?sig=SECRET&jwt=SECRET", http.StatusFound)
		}
	}))
	t.Cleanup(gh.Close)
	u := &Updater{Base: gh.URL + "/releases", Client: gh.Client(), Current: "v1.0.0"}
	v, err := u.Latest(context.Background())
	if err != nil || v != "v1.4.2" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestLatestFollowsTheRedirect(t *testing.T) {
	r := newRelease(t, "v1.2.0", "darwin")
	u := updater(t, r, r.server(t), "darwin")
	v, err := u.Latest(context.Background())
	if err != nil || v != "v1.2.0" || !u.Newer(v) {
		t.Fatalf("%v %v", v, err)
	}
	u.Current = "dev"
	if u.Newer(v) {
		t.Fatal("dev builds must not auto-update")
	}
}

func TestGoodReleaseInstallsAtomically(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			r := newRelease(t, "v1.2.0", goos)
			u := updater(t, r, r.server(t), goos)
			bin, err := u.Download(context.Background(), "v1.2.0")
			if err != nil {
				t.Fatal(err)
			}
			if err := u.Install(bin, "v1.2.0"); err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(u.Exe); string(b) != "NEW BINARY v1.2.0" {
				t.Fatalf("exe = %q", b)
			}
			entries, _ := os.ReadDir(filepath.Dir(u.Exe))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".tuzy-update-") {
					t.Fatal("temp file left behind")
				}
			}
			if goos == "windows" {
				olds, _ := filepath.Glob(u.Exe + ".old-*")
				if len(olds) != 1 {
					t.Fatalf("old exe not kept for cleanup: %v", olds)
				}
				if b, _ := os.ReadFile(olds[0]); string(b) != "OLD BINARY" {
					t.Fatal("wrong .old content")
				}
				CleanupOld(u.Exe)
				if olds, _ := filepath.Glob(u.Exe + ".old-*"); len(olds) != 0 {
					t.Fatal(".old not cleaned up")
				}
			}
		})
	}
}

func TestTamperedReleasesAreRefused(t *testing.T) {
	cases := map[string]func(r *release){
		"bad checksum":  func(r *release) { r.archive = append(r.archive, 0) },
		"bad signature": func(r *release) { r.sums = bytes.Replace(r.sums, []byte("other"), []byte("evil_"), 1) },
		"foreign key": func(r *release) {
			_, p, _ := ed25519.GenerateKey(rand.Reader)
			r.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(p, r.sums)))
		},
		"missing asset line": func(r *release) {
			r.sums = []byte("abc  nothing\n")
			r.sig = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(r.priv, r.sums)))
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRelease(t, "v1.2.0", "linux")
			tamper(r)
			u := updater(t, r, r.server(t), "linux")
			if _, err := u.Download(context.Background(), "v1.2.0"); err == nil {
				t.Fatal("tampered release accepted")
			}
			if b, _ := os.ReadFile(u.Exe); string(b) != "OLD BINARY" {
				t.Fatal("exe changed")
			}
		})
	}
	u := &Updater{}
	if _, err := u.Download(context.Background(), "v1.2.0"); err == nil || !strings.Contains(err.Error(), "no release key") {
		t.Fatalf("no keys: %v", err)
	}
}

func TestBrokenBinaryIsNotSwappedIn(t *testing.T) {
	r := newRelease(t, "v1.2.0", "linux")
	u := updater(t, r, r.server(t), "linux")
	u.VerifyRun = func(string) (string, error) { return "exec format error", fmt.Errorf("exit 126") }
	bin, err := u.Download(context.Background(), "v1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Install(bin, "v1.2.0"); err == nil {
		t.Fatal("broken binary installed")
	}
	if b, _ := os.ReadFile(u.Exe); string(b) != "OLD BINARY" {
		t.Fatal("exe changed")
	}
}

func TestDetectMethod(t *testing.T) {
	home := "/Users/me"
	cases := map[string]Method{
		"/opt/homebrew/Cellar/tuzy/1.0.0/bin/tuzy":     MethodBrew,
		"/usr/local/Cellar/tuzy/1.0.0/bin/tuzy":        MethodBrew,
		`C:/Users/me/scoop/apps/tuzy/current/tuzy.exe`: MethodScoop,
		"/Users/me/go/bin/tuzy":                        MethodGo,
		"/usr/local/bin/tuzy":                          MethodBinary,
		"/Users/me/.local/bin/tuzy":                    MethodBinary,
	}
	for exe, want := range cases {
		if got := DetectMethod(exe, "", "", home); got != want {
			t.Errorf("%s: %s, want %s", exe, got, want)
		}
	}
	if DetectMethod("/custom/gobin/tuzy", "", "/custom/gobin", home) != MethodGo {
		t.Error("GOBIN not detected")
	}
	if MethodBrew.Hint() != "brew upgrade tuzy" || MethodBinary.Hint() != "" {
		t.Error("hints")
	}
}

func TestNoticeOncePerDay(t *testing.T) {
	r := newRelease(t, "v1.2.0", "darwin")
	u := updater(t, r, r.server(t), "darwin")
	p := filepath.Join(t.TempDir(), "update.json")
	now := time.Now()
	RefreshNotice(context.Background(), u, p, now)
	n := len(r.requests)
	RefreshNotice(context.Background(), u, p, now.Add(time.Hour)) // cached: no request
	if len(r.requests) != n {
		t.Fatal("refreshed within 24 h")
	}
	if v := PendingNotice(u, p, now); v != "v1.2.0" {
		t.Fatalf("notice %q", v)
	}
	if v := PendingNotice(u, p, now.Add(time.Hour)); v != "" {
		t.Fatal("nagged twice in a day")
	}
	if v := PendingNotice(u, p, now.Add(25*time.Hour)); v != "v1.2.0" {
		t.Fatal("no notice the next day")
	}
	u.Current = "v1.2.0"
	if v := PendingNotice(u, p, now.Add(50*time.Hour)); v != "" {
		t.Fatal("notice although up to date")
	}
}
