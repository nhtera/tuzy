package ui

import "testing"

func TestSymbolsFallBackOnlyOnTheLegacyWindowsConsole(t *testing.T) {
	env := func(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }
	cases := []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"darwin", nil, "✓✗"},
		{"linux", nil, "✓✗"},
		{"windows", nil, "√×"},                                         // conhost: Windows PowerShell, cmd
		{"windows", map[string]string{"WT_SESSION": "1"}, "✓✗"},        // Windows Terminal
		{"windows", map[string]string{"TERM_PROGRAM": "vscode"}, "✓✗"}, // VS Code terminal
	}
	for _, c := range cases {
		if check, cross := symbols(c.goos, env(c.env)); check+cross != c.want {
			t.Errorf("%s %v: %s%s, want %s", c.goos, c.env, check, cross, c.want)
		}
	}
}
