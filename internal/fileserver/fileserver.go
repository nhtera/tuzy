// Package fileserver exposes a directory as a read-only static HTTP site (directory listing on,
// dotfiles hidden, symlink escapes blocked) behind an in-process http.RoundTripper — no TCP hop, so
// it plugs into the same relay path a real upstream uses (tunnel.BuildLocalRequest, the recorder,
// inspector replay) unchanged.
package fileserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// RoundTripper serves a directory rooted at the path given to New. dir must already be an
// absolute, symlink-resolved path: callers (internal/upstream) are responsible for deciding
// whether that path is allowed (see upstream.ResolveCLIDir / ResolveProjectDir).
type RoundTripper struct {
	handler http.Handler
}

// New opens dir (via os.OpenRoot, so symlinks that resolve outside dir — including any absolute
// symlink — are rejected at access time) and returns a RoundTripper that serves it read-only, with
// directory listing on and dotfiles hidden.
func New(dir string) (*RoundTripper, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	fsys := hideFS{root: root, fsys: root.FS()}
	return &RoundTripper{handler: http.FileServerFS(fsys)}, nil
}

// allowedMethods is the Allow header value for a 405 response.
const allowedMethods = "GET, HEAD"

// RoundTrip serves req against the directory, without a TCP hop: the handler runs in its own
// goroutine, writing into an io.Pipe that backs the returned response body, so large files and
// Range requests stream instead of being buffered whole.
//
// Only GET and HEAD are served (a static file server has no meaningful response to anything else).
// req.Context() is honoured for the lifetime of the returned response body: cancelling it aborts
// the handler goroutine (by failing its next Write) and makes further Body reads return the
// context's error. A panic inside the handler is recovered into a 500 (or, if headers were already
// sent, an aborted body) instead of taking the process down.
func (rt *RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return &http.Response{
			Status:        http.StatusText(http.StatusMethodNotAllowed),
			StatusCode:    http.StatusMethodNotAllowed,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Header:        http.Header{"Allow": []string{allowedMethods}},
			Body:          http.NoBody,
			ContentLength: 0,
			Request:       req,
		}, nil
	}

	pr, pw := io.Pipe()
	rw := &pipeResponseWriter{header: make(http.Header), status: http.StatusOK, pw: pw, headerDone: make(chan struct{})}

	// If req.Context() is (or becomes) done, closing the pipe's read side makes the handler's next
	// Write fail (unblocking/ending its goroutine) and makes any pending or future Body.Read return
	// ctx.Err() instead of hanging or silently returning a truncated 200.
	stop := context.AfterFunc(req.Context(), func() {
		_ = pr.CloseWithError(req.Context().Err())
	})

	go func() {
		defer func() {
			if p := recover(); p != nil {
				rw.abortOnPanic(p)
			}
			rw.finish()
		}()
		rt.handler.ServeHTTP(rw, req)
	}()
	<-rw.headerDone

	return &http.Response{
		Status:        http.StatusText(rw.status),
		StatusCode:    rw.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        rw.header,
		Body:          &cancelOnCloseBody{ReadCloser: pr, stop: stop},
		ContentLength: -1,
		Request:       req,
	}, nil
}

// cancelOnCloseBody deregisters the context.AfterFunc callback once the caller is done reading the
// response body, so a long-lived request context (e.g. the tunnel session) doesn't keep the
// callback registered for the rest of its life after this one response is finished.
type cancelOnCloseBody struct {
	io.ReadCloser
	stop func() bool
}

func (b *cancelOnCloseBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

// pipeResponseWriter is an http.ResponseWriter that streams its body straight into an io.Pipe, so
// RoundTrip can hand back a *http.Response as soon as headers are written instead of buffering the
// whole body (needed for large files and for Range responses to stream).
type pipeResponseWriter struct {
	header     http.Header
	pw         *io.PipeWriter
	status     int
	headerOnce sync.Once
	headerDone chan struct{}
	wroteHead  bool
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(status int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = status
	w.headerOnce.Do(func() { close(w.headerDone) })
}

func (w *pipeResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	return w.pw.Write(b)
}

// finish is called once the handler returns: it makes sure headerDone is closed even if the
// handler never wrote anything (e.g. a HEAD request with an empty body), and closes the pipe so
// the reader sees EOF.
func (w *pipeResponseWriter) finish() {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	_ = w.pw.Close()
}

// abortOnPanic recovers a handler panic: if no header was written yet, the caller still gets a
// clean 500; either way the pipe is closed with an error so the body never looks like a complete,
// successful response, and the process never crashes.
func (w *pipeResponseWriter) abortOnPanic(p any) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusInternalServerError)
	}
	_ = w.pw.CloseWithError(fmt.Errorf("fileserver: handler panic: %v", p))
}

// shortName8dot3 matches a Windows 8.3 short file name component (e.g. LONGFI~1.TXT), which can
// alias a hidden/dotfile-adjacent name that a plain "starts with a dot" check would miss.
var shortName8dot3 = regexp.MustCompile(`~[0-9]`)

// hiddenSegment reports whether one path element must never be served: dotfiles (and, on Windows,
// 8.3 short names).
func hiddenSegment(name string) bool {
	if name == "" || name == "." {
		return false
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	if runtime.GOOS == "windows" && shortName8dot3.MatchString(name) {
		return true
	}
	return false
}

// hidden reports whether an fs.FS path (always "/"-separated, "." for the root) contains a hidden
// element.
func hidden(name string) bool {
	if name == "." {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if hiddenSegment(seg) {
			return true
		}
	}
	return false
}

// maxSymlinkHops bounds how many in-root symlinks hiddenViaSymlink will follow while resolving one
// path, so a symlink cycle (a -> b -> a) can't hang the request.
const maxSymlinkHops = 40

// hiddenViaSymlink walks name (an fs.FS path: "/"-separated, "." for the root) component by
// component using root.Lstat, the same way os.Root itself resolves a path, but stopping to inspect
// every symlink along the way instead of blindly following it: a symlink's target is read with
// root.Readlink, resolved relative to the directory containing the symlink (standard symlink
// semantics — the same thing os.Root does internally), and the hidden-segment check is re-applied
// to every segment of the resolved path. This catches what hidden(name) alone cannot: an in-root
// symlink whose *target* is hidden (readme.txt -> .env, or a directory symlink into .git) even
// though the symlink's own name looks innocent.
//
// It reports false (not hidden) for any path root.Lstat can't resolve (missing entry, permission
// error, ...): those cases are left for the real Open to fail with its own, already-correct error.
func hiddenViaSymlink(root *os.Root, name string) bool {
	if name == "." {
		return false
	}
	remaining := strings.Split(name, "/")
	resolved := "" // path (relative to root) confirmed to be a chain of non-symlink components so far
	hops := 0
	for len(remaining) > 0 {
		seg := remaining[0]
		remaining = remaining[1:]
		if hiddenSegment(seg) {
			return true
		}
		candidate := seg
		if resolved != "" {
			candidate = resolved + "/" + seg
		}
		fi, err := root.Lstat(candidate)
		if err != nil {
			return false
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		hops++
		if hops > maxSymlinkHops {
			return true // unreasonably long/looping chain: refuse rather than spin
		}
		target, err := root.Readlink(candidate)
		if err != nil {
			return false
		}
		if filepath.IsAbs(target) {
			return true // os.Root rejects an absolute symlink target on open anyway; treat as blocked
		}
		next := path.Clean(path.Join(resolved, filepath.ToSlash(target)))
		if next == ".." || strings.HasPrefix(next, "../") {
			return true // escapes the root: os.Root would reject this on open too
		}
		resolved = ""
		if next == "." {
			continue
		}
		remaining = append(strings.Split(next, "/"), remaining...)
	}
	return false
}

// hideFS wraps an os.Root (and its fs.FS view) to (1) reject any path with a hidden element, or
// whose in-root symlink chain resolves through one, before ever calling Open, and (2) map any error
// Open returns other than "not found"/"permission" — in particular os.Root's opaque "path escapes
// from parent" error for a symlink that resolves outside the root, or is itself absolute — to
// fs.ErrNotExist, so an escape attempt looks exactly like a missing file: 404, never a 500, and
// never distinguishable from "doesn't exist".
type hideFS struct {
	root *os.Root
	fsys fs.FS
}

func (h hideFS) Open(name string) (fs.File, error) {
	if hidden(name) || hiddenViaSymlink(h.root, name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	f, err := h.fsys.Open(name)
	if err != nil {
		return nil, notFoundUnlessKnown(name, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, notFoundUnlessKnown(name, err)
	}
	if !fi.IsDir() {
		return f, nil
	}
	rd, ok := f.(fs.ReadDirFile)
	if !ok {
		_ = f.Close()
		return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("directory does not support listing")}
	}
	return &hideDirFile{File: f, rd: rd, root: h.root, dir: name}, nil
}

func notFoundUnlessKnown(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return err
	}
	return &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

// hideDirFile filters dot (and, on Windows, 8.3 short-name) entries, and symlinks that resolve
// through a hidden component, out of a directory listing.
type hideDirFile struct {
	fs.File
	rd      fs.ReadDirFile
	root    *os.Root
	dir     string // the listed directory's fs.FS path ("." for the root)
	entries []fs.DirEntry
	loaded  bool
}

// ReadDir implements fs.ReadDirFile per its documented contract: n <= 0 returns every remaining
// (filtered) entry in one slice; n > 0 returns at most n, and once none are left returns io.EOF.
func (d *hideDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.loaded {
		all, err := d.rd.ReadDir(-1)
		if err != nil {
			return nil, err
		}
		for _, e := range all {
			if hiddenSegment(e.Name()) {
				continue
			}
			entryPath := e.Name()
			if d.dir != "." && d.dir != "" {
				entryPath = d.dir + "/" + e.Name()
			}
			if e.Type()&fs.ModeSymlink != 0 && hiddenViaSymlink(d.root, entryPath) {
				continue
			}
			d.entries = append(d.entries, e)
		}
		d.loaded = true
	}
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, nil
	}
	if len(d.entries) == 0 {
		return nil, io.EOF
	}
	if n > len(d.entries) {
		n = len(d.entries)
	}
	out := d.entries[:n]
	d.entries = d.entries[n:]
	return out, nil
}
