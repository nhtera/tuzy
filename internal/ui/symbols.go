package ui

import (
	"os"
	"runtime"
)

// Check and Cross mark a result ("✓ Logged in as …"). The legacy Windows console (conhost, the
// default for Windows PowerShell and cmd) renders ✓/✗ as a box: its fonts lack them. √ and × are
// in every console font (the same fallback as the `figures` npm package).
var Check, Cross = symbols(runtime.GOOS, os.Getenv)

func symbols(goos string, getenv func(string) string) (check, cross string) {
	if goos == "windows" && getenv("WT_SESSION") == "" && getenv("TERM_PROGRAM") != "vscode" {
		return "√", "×"
	}
	return "✓", "✗"
}
