// Package config loads the user config ($UserConfigDir/tuzy/config.toml) and the per-project
// tuzy.toml. The project file only lists tunnels: it can never choose the server (a cloned repo
// must not be able to redirect your token) or a default name.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pelletier/go-toml/v2"

	"github.com/nhtera/tuzy/internal/names"
)

// DefaultServer is the production edge.
const DefaultServer = "https://tuzy.dev"

// ProjectFile is the per-project config file name.
const ProjectFile = "tuzy.toml"

// User is the per-user config.
type User struct {
	Server      string `toml:"server"`
	InspectAddr string `toml:"inspect_addr"`
	Log         string `toml:"log"`
}

// Tunnel is one [tunnels.<name>] entry of tuzy.toml.
type Tunnel struct {
	Addr       string `toml:"addr"`
	HostHeader string `toml:"host_header"`
}

// Project is a parsed tuzy.toml.
type Project struct {
	Tunnels map[string]Tunnel `toml:"tunnels"`
}

// UserPath returns the default user config path.
func UserPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "tuzy", "config.toml"), nil
}

// LoadUser reads the user config; a missing file yields the zero config.
func LoadUser(path string) (User, error) {
	var u User
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return u, nil
	}
	if err != nil {
		return u, err
	}
	if err := decodeStrict(b, &u); err != nil {
		return u, fmt.Errorf("%s: %w", path, err)
	}
	return u, nil
}

// forbiddenProjectKeys may never appear in tuzy.toml.
var forbiddenProjectKeys = map[string]string{
	"server":       "the server can only be set with --server, TUZY_SERVER or your user config",
	"default_name": "the default name is kept on the server: use `tuzy names default`",
	"token":        "tokens are stored in your OS keychain, never in project files",
}

// LoadProject reads and validates a tuzy.toml.
func LoadProject(path string) (Project, error) {
	var p Project
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	var raw map[string]any
	if err := toml.Unmarshal(b, &raw); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	for key, why := range forbiddenProjectKeys {
		if _, ok := raw[key]; ok {
			return p, fmt.Errorf("%s: %q is not allowed in %s (%s)", path, key, ProjectFile, why)
		}
	}
	if err := decodeStrict(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", path, err)
	}
	for name, t := range p.Tunnels {
		if !names.Valid(name) {
			return p, fmt.Errorf("%s: invalid tunnel name %q (3-32 chars of a-z, 0-9, -)", path, name)
		}
		if t.Addr == "" {
			return p, fmt.Errorf("%s: tunnels.%s.addr is required", path, name)
		}
		if t.HostHeader != "" && t.HostHeader != "preserve" && t.HostHeader != "rewrite" {
			return p, fmt.Errorf("%s: tunnels.%s.host_header must be \"preserve\" or \"rewrite\"", path, name)
		}
	}
	return p, nil
}

// Names returns the tunnel names in a stable order.
func (p Project) Names() []string {
	out := make([]string, 0, len(p.Tunnels))
	for n := range p.Tunnels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func decodeStrict(b []byte, v any) error {
	d := toml.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	return d.Decode(v)
}
