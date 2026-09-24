package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func spec(t *testing.T, goos string) Spec {
	t.Helper()
	home := t.TempDir()
	proj := filepath.Join(home, "my app", "tuzy.toml")
	if err := os.MkdirAll(filepath.Dir(proj), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proj, []byte("[tunnels.x]\naddr = 3000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Host-absolute paths: in real use Spec.GOOS is always the host OS.
	return Spec{GOOS: goos, Exe: filepath.Join(filepath.VolumeName(home)+string(filepath.Separator), "opt", "tuzy", "tuzy"), ProjectFile: proj, LogPath: filepath.Join(home, "logs", "tuzy.log"), Home: home, UID: 501}
}

func TestRenderPlist(t *testing.T) {
	s := spec(t, "darwin")
	s.Exe = "/opt/tuzy & co/tuzy"
	out, err := s.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>Label</key><string>dev.tuzy.agent</string>",
		"<string>/opt/tuzy &amp; co/tuzy</string>",
		"<string>start</string>\n    <string>--all</string>\n    <string>--file</string>\n    <string>" + s.ProjectFile + "</string>",
		"<string>--log-format</string>\n    <string>json</string>",
		"<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>",
		"<key>WorkingDirectory</key><string>" + filepath.Dir(s.ProjectFile) + "</string>",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("plist missing %q:\n%s", want, out)
		}
	}
	if s.UnitPath() != filepath.Join(s.Home, "Library/LaunchAgents/dev.tuzy.agent.plist") {
		t.Fatal(s.UnitPath())
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	s := spec(t, "linux")
	s.Exe = `/home/me/bin/tu"zy$%`
	out, err := s.Render()
	if err != nil {
		t.Fatal(err)
	}
	want := `ExecStart="/home/me/bin/tu\"zy$$%%" "start" "--all" "--file" ` + systemdQuote(s.ProjectFile) + ` "--log" ` + systemdQuote(s.LogPath) + ` "--log-format" "json" "--quiet"`
	// WorkingDirectory must NOT be quoted: systemd rejects `"/abs"` as a non-absolute path.
	for _, w := range []string{want, "Restart=always", "WantedBy=default.target", "\nWorkingDirectory=" + filepath.Dir(s.ProjectFile) + "\n"} {
		if !strings.Contains(out, w) {
			t.Fatalf("unit missing %q:\n%s", w, out)
		}
	}
}

func TestRenderWindowsTaskXML(t *testing.T) {
	s := Spec{GOOS: "windows", Exe: `C:\Program Files\tuzy\tuzy.exe`, ProjectFile: `C:\Users\Me\my app\tuzy.toml`, LogPath: `C:\Users\Me\AppData\Local\tuzy\tuzy.log`, User: `PC\Me & Co`}
	out, err := s.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<LogonTrigger><Enabled>true</Enabled><UserId>PC\Me &amp; Co</UserId></LogonTrigger>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`<Hidden>true</Hidden>`, `<RestartOnFailure>`,
		`<Command>%SystemRoot%\System32\conhost.exe</Command>`,
		`<Arguments>--headless &#34;C:\Program Files\tuzy\tuzy.exe&#34; start --all --file &#34;C:\Users\Me\my app\tuzy.toml&#34; --log C:\Users\Me\AppData\Local\tuzy\tuzy.log --log-format json --quiet</Arguments>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("task XML missing %q:\n%s", want, out)
		}
	}
	if b := utf16LE("A"); len(b) != 4 || b[0] != 0xFF || b[1] != 0xFE || b[2] != 'A' {
		t.Fatalf("utf16 %v", b)
	}
	if windowsQuote(`a "b"\`) != `"a \"b\"\\"` {
		t.Fatal(windowsQuote(`a "b"\`))
	}
}

func TestValidateRefusesTempBuildsAndRelativePaths(t *testing.T) {
	s := spec(t, "darwin")
	s.Exe = filepath.Join(os.TempDir(), "go-build123", "exe", "tuzy")
	if err := s.Validate(); err == nil {
		t.Fatal("temp build accepted")
	}
	s = spec(t, "darwin")
	s.ProjectFile = "tuzy.toml"
	if err := s.Validate(); err == nil {
		t.Fatal("relative path accepted")
	}
	s = spec(t, "linux")
	s.ProjectFile = "/tmp/x\nExecStartPre=/bin/sh -c evil/tuzy.toml"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("newline injection accepted: %v", err)
	}
}

func TestStableExe(t *testing.T) {
	cases := map[string]string{
		"/opt/homebrew/Cellar/tuzy/1.2.3/bin/tuzy":              "/opt/homebrew/opt/tuzy/bin/tuzy",
		"/home/linuxbrew/.linuxbrew/Cellar/tuzy/1.2.3/bin/tuzy": "/home/linuxbrew/.linuxbrew/opt/tuzy/bin/tuzy",
		"/Users/me/scoop/apps/tuzy/1.2.3/tuzy.exe":              "/Users/me/scoop/apps/tuzy/current/tuzy.exe",
		"/usr/local/bin/tuzy":                                   "/usr/local/bin/tuzy",
		"/opt/homebrew/Caskroom/tuzy/1.2.3/tuzy":                "/opt/homebrew/bin/tuzy",
	}
	for in, want := range cases {
		if got := filepath.ToSlash(StableExe(in)); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
}

type fakeRun struct {
	calls []string
	fail  map[string]bool
	out   map[string]string
	hook  func(f *fakeRun, c string) // runs before each call is answered
}

func (f *fakeRun) run(name string, args ...string) (string, error) {
	c := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, c)
	if f.hook != nil {
		f.hook(f, c)
	}
	for k := range f.fail {
		if strings.HasPrefix(c, k) {
			return "", errors.New("exit 1")
		}
	}
	for k, v := range f.out {
		if strings.HasPrefix(c, k) {
			return v, nil
		}
	}
	return "", nil
}

func TestLinuxLifecycle(t *testing.T) {
	f := &fakeRun{}
	m := Manager{Spec: spec(t, "linux"), Run: f.run}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Spec.UnitPath()); err != nil {
		t.Fatal(err)
	}
	_ = m.Start()
	_ = m.Restart()
	_ = m.Stop()
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl --user daemon-reload", "systemctl --user enable tuzy.service", "systemctl --user start tuzy.service",
		"systemctl --user restart tuzy.service", "systemctl --user stop tuzy.service",
		"systemctl --user disable --now tuzy.service", "systemctl --user daemon-reload",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	if m.Status() != "not installed" {
		t.Fatal(m.Status())
	}
}

func TestDarwinLifecycleAndStatus(t *testing.T) {
	f := &fakeRun{fail: map[string]bool{"launchctl print": true}}
	m := Manager{Spec: spec(t, "darwin"), Run: f.run}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err != nil { // not loaded yet → bootstrap
		t.Fatal(err)
	}
	if got := f.calls[len(f.calls)-1]; got != "launchctl bootstrap gui/501 "+m.Spec.UnitPath() {
		t.Fatal(got)
	}
	f.fail = nil
	f.out = map[string]string{"launchctl print": "gui/501/dev.tuzy.agent = {\n\tstate = running\n\tpid = 4242\n}"}
	if s := m.Status(); s != "running (pid 4242)" {
		t.Fatal(s)
	}
	// Restart = bootout + bootstrap so a re-installed plist takes effect. bootout returns while the
	// job is still draining: the bootstrap must wait until launchd has unloaded it.
	unloadPoll = time.Millisecond
	t.Cleanup(func() { unloadPoll = 100 * time.Millisecond })
	polls := -1 // after bootout, `print` keeps reporting "loaded" for 2 polls
	f.hook = func(f *fakeRun, c string) {
		switch {
		case strings.HasPrefix(c, "launchctl bootout"):
			polls = 2
		case strings.HasPrefix(c, "launchctl print") && polls >= 0:
			if polls == 0 {
				f.fail = map[string]bool{"launchctl print": true}
			}
			polls--
		}
	}
	f.calls = nil
	if err := m.Restart(); err != nil {
		t.Fatal(err)
	}
	target := "gui/501/dev.tuzy.agent"
	want := []string{"launchctl print " + target, "launchctl bootout " + target,
		"launchctl print " + target, "launchctl print " + target, "launchctl print " + target, // wait for unload
		"launchctl print " + target, "launchctl bootstrap gui/501 " + m.Spec.UnitPath()}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(f.calls, "\n"))
	}
	f.hook, f.fail = nil, nil
	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.Spec.UnitPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plist not removed")
	}
}

func TestDarwinStopTimesOutIfNeverUnloaded(t *testing.T) {
	unloadPoll, unloadTimeout = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { unloadPoll, unloadTimeout = 100*time.Millisecond, 30*time.Second })
	m := Manager{Spec: spec(t, "darwin"), Run: (&fakeRun{}).run} // `print` always succeeds: stuck
	if err := m.Stop(); err == nil || !strings.Contains(err.Error(), "still shutting down") {
		t.Fatal(err)
	}
}

func TestWindowsCommands(t *testing.T) {
	s := spec(t, "windows")
	f := &fakeRun{out: map[string]string{"schtasks /Query": `"\tuzy","N/A","Running"`}}
	m := Manager{Spec: s, Run: f.run}
	if err := m.Install(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.calls[0], "schtasks /Create /XML ") || !strings.HasSuffix(f.calls[0], "tuzy-task.xml /TN tuzy /F") {
		t.Fatal(f.calls[0])
	}
	if m.Status() != "running" {
		t.Fatal(m.Status())
	}
}
