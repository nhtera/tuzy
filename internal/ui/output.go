// Package ui renders tunnel status and the access log for the terminal.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nhtera/tuzy/internal/tunnel"
)

// Printer writes status lines and access-log lines. Safe for concurrent use (several tunnels).
type Printer struct {
	mu     sync.Mutex
	out    io.Writer
	quiet  bool // suppress the access log
	color  bool
	server string // printed when not the default server
}

// New returns a printer for w. Colors are used only on a terminal without NO_COLOR.
func New(w io.Writer, quiet bool, server string) *Printer {
	return &Printer{out: w, quiet: quiet, color: isTerminal(w) && os.Getenv("NO_COLOR") == "", server: server}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

const (
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	red    = "\x1b[31m"
	dim    = "\x1b[2m"
	reset  = "\x1b[0m"
)

func (p *Printer) paint(c, s string) string {
	if !p.color {
		return s
	}
	return c + s + reset
}

func (p *Printer) line(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = fmt.Fprintf(p.out, format+"\n", a...)
}

// Event prints a tunnel state change. target is the local URL for the online line.
func (p *Printer) Event(e tunnel.Event, target string) {
	switch e.Kind {
	case tunnel.EventOnline:
		p.line("%s %s → %s", p.paint(green, "● online"), e.URL, target)
		if p.server != "" {
			p.line("  server: %s", p.server)
		}
	case tunnel.EventReconnecting:
		reason := ""
		if e.Err != nil {
			reason = " (" + e.Err.Error() + ")"
		}
		p.line("%s %s: reconnecting in %s%s", p.paint(yellow, "○"), e.Name, e.RetryIn.Round(10*time.Millisecond), reason)
	case tunnel.EventRenamed:
		p.line("%s %s was renamed to %s; update tuzy.toml if it lists the old name", p.paint(yellow, "↻"), e.OldName, e.Name)
	}
}

// Access prints one access-log line unless quiet.
func (p *Printer) Access(name string, a tunnel.AccessEntry) {
	if p.quiet {
		return
	}
	status := fmt.Sprint(a.Status)
	switch {
	case a.Status == 0:
		status = p.paint(red, "---")
	case a.Status >= 500:
		status = p.paint(red, status)
	case a.Status >= 400:
		status = p.paint(yellow, status)
	default:
		status = p.paint(green, status)
	}
	extra := ""
	if a.Err != "" && a.Err != "cancelled" {
		extra = " " + p.paint(dim, a.Err)
	}
	prefix := ""
	if name != "" {
		prefix = p.paint(dim, name+" ")
	}
	p.line("%s%s %-6s %s %s %s%s", prefix, time.Now().Format("15:04:05"), sanitize(a.Method), sanitize(a.Path), status, a.Duration.Round(time.Millisecond), extra)
}

// sanitize neutralizes control characters (terminal escape injection via visitor paths).
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}
