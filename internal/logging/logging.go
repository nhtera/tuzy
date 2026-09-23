// Package logging writes structured tunnel logs (`--log`, `--log-format`, `--log-level`) with
// log/slog. Formats: term (compact, human), logfmt (key=value) and json. Event names:
//
//	tunnel.online   name url target epoch
//	tunnel.reconnecting name retry_in error
//	tunnel.renamed  name old_name
//	request         tunnel method path status duration_ms bytes remote_ip [error]
//
// Bodies and headers are never logged.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nhtera/tuzy/internal/tunnel"
)

// MaxFileBytes is the size above which a log file is rotated at start to <path>.1 (no rotation library).
const MaxFileBytes = 50 << 20

// Options select the destination, format and level.
type Options struct {
	Dest   string // "stdout" | "stderr" | <path> | "false" / "" (disabled)
	Format string // "term" (default) | "logfmt" | "json"
	Level  string // "debug" | "info" (default) | "warn" | "error"
}

// Logger emits tunnel events and access entries. A nil *Logger is valid and logs nothing.
type Logger struct {
	log    *slog.Logger
	closer io.Closer
}

// Enabled reports whether o turns logging on.
func (o Options) Enabled() bool { return o.Dest != "" && o.Dest != "false" }

// Validate checks the option values.
func (o Options) Validate() error {
	switch o.Format {
	case "", "term", "logfmt", "json":
	default:
		return fmt.Errorf("--log-format must be term, logfmt or json, not %q", o.Format)
	}
	if _, err := parseLevel(o.Level); err != nil {
		return err
	}
	return nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("--log-level must be debug, info, warn or error, not %q", s)
}

// Open returns a logger for o (nil when disabled). stdout/stderr are the process streams.
func Open(o Options, stdout, stderr io.Writer) (*Logger, error) {
	if !o.Enabled() {
		return nil, nil
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	level, _ := parseLevel(o.Level)
	var w io.Writer
	var closer io.Closer
	switch o.Dest {
	case "stdout":
		w = stdout
	case "stderr":
		w = stderr
	default:
		f, err := openFile(o.Dest)
		if err != nil {
			return nil, err
		}
		w, closer = f, f
	}
	hopts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch o.Format {
	case "json":
		h = slog.NewJSONHandler(w, hopts)
	case "logfmt":
		h = slog.NewTextHandler(w, hopts)
	default:
		h = &termHandler{w: w, level: level, mu: &sync.Mutex{}}
	}
	return &Logger{log: slog.New(h), closer: closer}, nil
}

func openFile(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > MaxFileBytes {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	return f, nil
}

// Close flushes and closes a file destination.
func (l *Logger) Close() error {
	if l == nil || l.closer == nil {
		return nil
	}
	return l.closer.Close()
}

// Info logs a free-form message (startup lines etc.).
func (l *Logger) Info(msg string, args ...any) {
	if l != nil {
		l.log.Info(msg, args...)
	}
}

// Error logs an error message.
func (l *Logger) Error(msg string, args ...any) {
	if l != nil {
		l.log.Error(msg, args...)
	}
}

// Event logs a tunnel state change.
func (l *Logger) Event(e tunnel.Event, target string) {
	if l == nil {
		return
	}
	switch e.Kind {
	case tunnel.EventOnline:
		l.log.Info("tunnel.online", "name", e.Name, "url", e.URL, "target", target, "epoch", e.Epoch)
	case tunnel.EventReconnecting:
		args := []any{"name", e.Name, "retry_in", e.RetryIn.Round(time.Millisecond).String()}
		if e.Err != nil {
			args = append(args, "error", e.Err.Error())
		}
		l.log.Warn("tunnel.reconnecting", args...)
	case tunnel.EventRenamed:
		l.log.Warn("tunnel.renamed", "name", e.Name, "old_name", e.OldName)
	default:
		l.log.Debug("tunnel.event", "name", e.Name, "kind", fmt.Sprint(e.Kind))
	}
}

// Access logs one proxied request (status 0 = no response was sent).
func (l *Logger) Access(name string, a tunnel.AccessEntry) {
	if l == nil {
		return
	}
	args := []any{"tunnel", name, "method", a.Method, "path", a.Path, "status", a.Status,
		"duration_ms", a.Duration.Milliseconds(), "bytes", a.Bytes, "remote_ip", a.RemoteIP}
	if a.Kind == "ws" {
		args = append(args, "kind", "ws")
	}
	level := slog.LevelInfo
	if a.Err != "" && a.Err != "cancelled" {
		args = append(args, "error", a.Err)
		level = slog.LevelWarn
	}
	l.log.Log(context.Background(), level, "request", args...)
}

// termHandler: "15:04:05 INF request tunnel=shop method=GET path=/ status=200 …".
type termHandler struct {
	w     io.Writer
	level slog.Level
	mu    *sync.Mutex
	attrs []slog.Attr
}

func (h *termHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *termHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(levelTag(r.Level))
	b.WriteByte(' ')
	b.WriteString(quoteIfNeeded(r.Message))
	write := func(a slog.Attr) bool {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(quoteIfNeeded(a.Value.Resolve().String()))
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *termHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

func (h *termHandler) WithGroup(string) slog.Handler { return h }

func levelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERR"
	case l >= slog.LevelWarn:
		return "WRN"
	case l >= slog.LevelInfo:
		return "INF"
	}
	return "DBG"
}

// quoteIfNeeded quotes values with spaces, quotes, '=', control, format (bidi) or line/paragraph
// separator characters (log injection and terminal spoofing); strconv.Quote escapes them.
func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	for _, r := range s {
		if r <= ' ' || r == '"' || r == '=' || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return strconv.Quote(s)
		}
	}
	return s
}
