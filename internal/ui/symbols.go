package ui

import (
	"os"
	"runtime"
)

// The legacy Windows console (conhost, the default for Windows PowerShell and cmd) renders ✓/✗ as
// a box: its fonts lack them. √ and × are in every console font (the same fallback as the
// `figures` npm package).
var check, cross = symbols(runtime.GOOS, os.Getenv)

// Check marks a success ("✓ Logged in as …").
var Check = check

// Cross marks a failure.
var Cross = cross

func symbols(goos string, getenv func(string) string) (check, cross string) {
	if goos == "windows" && getenv("WT_SESSION") == "" && getenv("TERM_PROGRAM") != "vscode" {
		return "√", "×"
	}
	return "✓", "✗"
}
