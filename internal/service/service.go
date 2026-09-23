// Package service runs `tuzy start --all` as a user-level background service without extra
// dependencies: a launchd LaunchAgent (macOS), a systemd --user unit (Linux) or a logon Scheduled
// Task (Windows; runs as the user, so the Credential Manager token works). All three start at
// login, keep the tunnels up and write JSON logs to a file.
package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"unicode/utf16"
)

// Label identifies the service on every platform.
const Label = "dev.tuzy.agent"

// Spec describes the service to install.
type Spec struct {
	GOOS        string // runtime.GOOS
	Exe         string // absolute (see StableExe)
	ProjectFile string // absolute path of tuzy.toml
	LogPath     string // absolute
	Home        string
	UID         int
	User        string   // Windows: DOMAIN\user for the logon trigger
	ExtraArgs   []string // e.g. --server/--config the user chose (the service must use the same)
}

// Args is the command line the service runs (--quiet: the JSON log is the only request log).
func (s Spec) Args() []string {
	a := []string{"start", "--all", "--file", s.ProjectFile, "--log", s.LogPath, "--log-format", "json", "--quiet"}
	return append(a, s.ExtraArgs...)
}

// StableExe maps a package manager's versioned path to its version-independent link, so the
// service survives `brew upgrade` / `scoop update` (the versioned directory is removed then).
func StableExe(exe string) string {
	p := filepath.ToSlash(exe)
	if i := strings.Index(p, "/Cellar/tuzy/"); i >= 0 {
		rest := p[i+len("/Cellar/tuzy/"):]
		if j := strings.Index(rest, "/"); j >= 0 {
			return filepath.FromSlash(p[:i] + "/opt/tuzy" + rest[j:])
		}
	}
	lower := strings.ToLower(p)
	if i := strings.Index(lower, "/scoop/apps/tuzy/"); i >= 0 {
		rest := p[i+len("/scoop/apps/tuzy/"):]
		if j := strings.Index(rest, "/"); j >= 0 {
			return filepath.FromSlash(p[:i] + "/scoop/apps/tuzy/current" + rest[j:])
		}
	}
	return exe
}

// UnitPath is where the plist/unit lives ("" on Windows: the task lives in the Task Scheduler).
func (s Spec) UnitPath() string {
	switch s.GOOS {
	case "darwin":
		return filepath.Join(s.Home, "Library", "LaunchAgents", Label+".plist")
	case "linux":
		return filepath.Join(s.Home, ".config", "systemd", "user", "tuzy.service")
	}
	return ""
}

// DefaultLogPath is the per-OS log location.
func DefaultLogPath(goos, home string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Logs", "tuzy", "tuzy.log")
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "tuzy", "tuzy.log")
		}
		return filepath.Join(home, "AppData", "Local", "tuzy", "tuzy.log")
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "tuzy", "tuzy.log")
	}
	return filepath.Join(home, ".local", "state", "tuzy", "tuzy.log")
}

// Validate refuses paths that would break after install (temp builds, relative paths).
func (s Spec) Validate() error {
	for _, p := range append([]string{s.Exe, s.ProjectFile, s.LogPath}, s.ExtraArgs...) {
		if strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("service paths must not contain control characters: %q", p)
		}
	}
	for _, p := range []string{s.Exe, s.ProjectFile, s.LogPath} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("service paths must be absolute, got %q", p)
		}
	}
	tmp := filepath.Clean(os.TempDir())
	if strings.HasPrefix(filepath.Clean(s.Exe), tmp+string(filepath.Separator)) || strings.Contains(s.Exe, "go-build") {
		return fmt.Errorf("%s looks like a temporary build (go run?); install tuzy first", s.Exe)
	}
	if _, err := os.Stat(s.ProjectFile); err != nil {
		return fmt.Errorf("project file: %w", err)
	}
	return nil
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"x": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{x .Exe}}</string>
{{- range .Args}}
    <string>{{x .}}</string>
{{- end}}
  </array>
  <key>WorkingDirectory</key><string>{{x .Dir}}</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>{{x .Out}}</string>
  <key>StandardErrorPath</key><string>{{x .Out}}</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
`))

// systemdQuote quotes one ExecStart word (systemd.syntax: C-style escapes in double quotes).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// systemdPath is a path setting (WorkingDirectory=, append:): NOT quoted (systemd would take the
// quote as part of a relative path and reject the unit); only specifiers are escaped.
func systemdPath(s string) string { return strings.ReplaceAll(s, "%", "%%") }

var unitTmpl = template.Must(template.New("unit").Parse(`[Unit]
Description=tuzy tunnels
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={{.ExecStart}}
WorkingDirectory={{.Dir}}
Restart=always
RestartSec=5
StandardOutput=append:{{.Out}}
StandardError=append:{{.Out}}

[Install]
WantedBy=default.target
`))

// windowsQuote quotes one argument for the task's command line (CommandLineToArgvW rules).
func windowsQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, 2*slashes+1))
			slashes = 0
			b.WriteRune(r)
			continue
		default:
			slashes = 0
		}
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// Render returns the unit/plist content, or the Windows task command line.
func (s Spec) Render() (string, error) {
	dir := filepath.Dir(s.ProjectFile)
	var buf bytes.Buffer
	switch s.GOOS {
	case "darwin":
		err := plistTmpl.Execute(&buf, map[string]any{"Label": Label, "Exe": s.Exe, "Args": s.Args(), "Dir": dir, "Out": s.LogPath + ".stdout"})
		return buf.String(), err
	case "linux":
		words := []string{systemdQuote(s.Exe)}
		for _, a := range s.Args() {
			words = append(words, systemdQuote(a))
		}
		err := unitTmpl.Execute(&buf, map[string]any{"ExecStart": strings.Join(words, " "), "Dir": systemdPath(dir), "Out": systemdPath(s.LogPath + ".stdout")})
		return buf.String(), err
	case "windows":
		// Hidden console: conhost --headless runs the console app without a window (closing a
		// visible one would kill the tunnels).
		args := []string{"--headless", windowsQuote(s.Exe)}
		for _, a := range s.Args() {
			args = append(args, windowsQuote(a))
		}
		err := taskTmpl.Execute(&buf, map[string]any{"User": s.User, "Args": strings.Join(args, " "), "Dir": dir})
		return buf.String(), err
	}
	return "", fmt.Errorf("tuzy service isn't supported on %s; run `tuzy start --all` from your own supervisor", s.GOOS)
}

// taskTmpl: the Task Scheduler definition. Unlike `schtasks /Create /SC ONLOGON`, it lifts the
// 3-day limit, the on-battery stop and the 261-char /TR limit, restarts on failure and hides.
var taskTmpl = template.Must(template.New("task").Funcs(template.FuncMap{"x": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>tuzy tunnels</Description></RegistrationInfo>
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>{{x .User}}</UserId></LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>{{x .User}}</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <StartWhenAvailable>true</StartWhenAvailable>
    <Hidden>true</Hidden>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>%SystemRoot%\System32\conhost.exe</Command><Arguments>{{x .Args}}</Arguments><WorkingDirectory>{{x .Dir}}</WorkingDirectory></Exec>
  </Actions>
</Task>
`))

// utf16LE encodes s as UTF-16 LE with a BOM (what `schtasks /XML` expects).
func utf16LE(s string) []byte {
	out := []byte{0xFF, 0xFE}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

// Runner executes a command (swapped in tests).
type Runner func(name string, args ...string) (string, error)

// ExecRunner runs real commands.
func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Manager performs service actions for one Spec.
type Manager struct {
	Spec Spec
	Run  Runner
}

func (m Manager) domain() string { return fmt.Sprintf("gui/%d", m.Spec.UID) }
func (m Manager) target() string { return m.domain() + "/" + Label }
func (m Manager) loaded() bool   { _, err := m.Run("launchctl", "print", m.target()); return err == nil }
func (m Manager) sc(a ...string) error {
	_, err := m.Run("systemctl", append([]string{"--user"}, a...)...)
	return err
}

// Install writes the unit (and registers it) without starting it.
func (m Manager) Install() error {
	if err := m.Spec.Validate(); err != nil {
		return err
	}
	content, err := m.Spec.Render()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.Spec.LogPath), 0o700); err != nil {
		return err
	}
	switch m.Spec.GOOS {
	case "windows":
		xmlPath := filepath.Join(filepath.Dir(m.Spec.LogPath), "tuzy-task.xml")
		if err := os.WriteFile(xmlPath, utf16LE(content), 0o600); err != nil {
			return err
		}
		defer func() { _ = os.Remove(xmlPath) }()
		_, err := m.Run("schtasks", "/Create", "/XML", xmlPath, "/TN", "tuzy", "/F")
		return err
	case "darwin", "linux":
		p := m.Spec.UnitPath()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
		if m.Spec.GOOS == "linux" {
			if err := m.sc("daemon-reload"); err != nil {
				return err
			}
			return m.sc("enable", "tuzy.service")
		}
		if m.loaded() { // re-install: drop the old definition; `service start` loads the new one
			_, _ = m.Run("launchctl", "bootout", m.target())
		}
		return nil
	}
	return nil
}

// Uninstall stops and removes the service (idempotent).
func (m Manager) Uninstall() error {
	switch m.Spec.GOOS {
	case "windows":
		_, _ = m.Run("schtasks", "/End", "/TN", "tuzy")
		_, err := m.Run("schtasks", "/Delete", "/TN", "tuzy", "/F")
		return err
	case "darwin":
		_, _ = m.Run("launchctl", "bootout", m.target())
	case "linux":
		_ = m.sc("disable", "--now", "tuzy.service")
	}
	if err := os.Remove(m.Spec.UnitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if m.Spec.GOOS == "linux" {
		return m.sc("daemon-reload")
	}
	return nil
}

// Start starts the service (loading it first on macOS).
func (m Manager) Start() error {
	switch m.Spec.GOOS {
	case "windows":
		_, err := m.Run("schtasks", "/Run", "/TN", "tuzy")
		return err
	case "darwin":
		if !m.loaded() {
			_, err := m.Run("launchctl", "bootstrap", m.domain(), m.Spec.UnitPath())
			return err
		}
		_, err := m.Run("launchctl", "kickstart", m.target())
		return err
	}
	return m.sc("start", "tuzy.service")
}

// Stop stops the service until the next Start (or login).
func (m Manager) Stop() error {
	switch m.Spec.GOOS {
	case "windows":
		_, err := m.Run("schtasks", "/End", "/TN", "tuzy")
		return err
	case "darwin":
		if !m.loaded() {
			return nil
		}
		_, err := m.Run("launchctl", "bootout", m.target()) // KeepAlive would restart a plain kill
		return err
	}
	return m.sc("stop", "tuzy.service")
}

// Restart restarts the service.
func (m Manager) Restart() error {
	switch m.Spec.GOOS {
	case "darwin":
		// bootout + bootstrap (not kickstart -k): picks up a re-installed plist.
		if err := m.Stop(); err != nil {
			return err
		}
		return m.Start()
	case "linux":
		return m.sc("restart", "tuzy.service")
	}
	_ = m.Stop()
	return m.Start()
}

// Status is a one-line state ("running (pid 123)", "stopped", "not installed").
func (m Manager) Status() string {
	switch m.Spec.GOOS {
	case "windows":
		out, err := m.Run("schtasks", "/Query", "/TN", "tuzy", "/FO", "CSV", "/NH")
		if err != nil {
			return "not installed"
		}
		f := strings.Split(strings.TrimSpace(out), ",")
		return strings.ToLower(strings.Trim(f[len(f)-1], `"`))
	case "darwin":
		if _, err := os.Stat(m.Spec.UnitPath()); err != nil {
			return "not installed"
		}
		out, err := m.Run("launchctl", "print", m.target())
		if err != nil {
			return "installed, not loaded (run `tuzy service start`)"
		}
		state, pid := "", ""
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			if v, ok := strings.CutPrefix(l, "state = "); ok && state == "" {
				state = v
			}
			if v, ok := strings.CutPrefix(l, "pid = "); ok && pid == "" {
				pid = v
			}
		}
		if pid != "" {
			return fmt.Sprintf("%s (pid %s)", state, pid)
		}
		return state
	}
	if _, err := os.Stat(m.Spec.UnitPath()); err != nil {
		return "not installed"
	}
	out, _ := m.Run("systemctl", "--user", "is-active", "tuzy.service")
	return strings.TrimSpace(out)
}
