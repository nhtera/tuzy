package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tuzy.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadProjectValid(t *testing.T) {
	p, err := LoadProject(write(t, `
[tunnels.web]
addr = "3000"

[tunnels.api-v2]
addr = "http://localhost:8080"
host_header = "rewrite"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.Names(), ","); got != "api-v2,web" {
		t.Fatalf("names = %s", got)
	}
	if p.Tunnels["api-v2"].HostHeader != "rewrite" {
		t.Fatal("host_header not parsed")
	}
}

func TestLoadProjectRejects(t *testing.T) {
	cases := map[string]string{
		"server key":       "server = \"https://evil.example\"\n[tunnels.web]\naddr = \"3000\"",
		"default_name key": "default_name = \"web\"\n[tunnels.web]\naddr = \"3000\"",
		"token key":        "token = \"x\"",
		"bad name":         "[tunnels.Bad_Name]\naddr = \"3000\"",
		"missing addr":     "[tunnels.web]\nhost_header = \"rewrite\"",
		"bad host_header":  "[tunnels.web]\naddr = \"3000\"\nhost_header = \"keep\"",
		"unknown field":    "[tunnels.web]\naddr = \"3000\"\nport = 3",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadProject(write(t, content)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLoadUserMissingIsZero(t *testing.T) {
	u, err := LoadUser(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || u != (User{}) {
		t.Fatalf("u=%+v err=%v", u, err)
	}
}

func TestLoadUserStrict(t *testing.T) {
	p := write(t, "server = \"http://localhost:8787\"\ninspect_addr = \"127.0.0.1:4040\"\n")
	u, err := LoadUser(p)
	if err != nil || u.Server != "http://localhost:8787" {
		t.Fatalf("u=%+v err=%v", u, err)
	}
	if _, err := LoadUser(write(t, "sever = \"typo\"")); err == nil {
		t.Fatal("unknown key should fail")
	}
}
