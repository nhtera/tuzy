package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCmdPrintsBuildMetadata(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{"tuzy " + buildVersion(), commit, date} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
}

func TestVersionCmdRejectsArgs(t *testing.T) {
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"version", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for extra args")
	}
}
