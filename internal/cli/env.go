package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/config"
)

// runtimeEnv is what every networked command resolves first.
type runtimeEnv struct {
	server       *url.URL
	serverSource string // "flag" | "env" | "config" | "default"
	token        string
	tokenRefused error // TUZY_TOKEN set but not allowed for this server (see resolveEnv)
	user         config.User
}

// resolveEnv picks the server and token. The server comes only from --server, TUZY_SERVER or the
// user config (never from a project tuzy.toml), and must be https unless it is local.
func resolveEnv(cmd *cobra.Command) (*runtimeEnv, error) {
	path, _ := cmd.Flags().GetString("config")
	if path == "" {
		p, err := config.UserPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	user, err := config.LoadUser(path)
	if err != nil {
		return nil, err
	}

	raw, source := config.DefaultServer, "default"
	if f, _ := cmd.Flags().GetString("server"); f != "" {
		raw, source = f, "flag"
	} else if e := os.Getenv("TUZY_SERVER"); e != "" {
		raw, source = e, "env"
	} else if user.Server != "" {
		raw, source = user.Server, "config"
	}
	server, err := parseServer(raw)
	if err != nil {
		return nil, fmt.Errorf("server (%s): %w", source, err)
	}
	token := os.Getenv("TUZY_TOKEN")
	// TUZY_TOKEN (typically a CI secret) only goes to the default server, a server named on the
	// command line, or a loopback dev server — never to one picked up from TUZY_SERVER / user
	// config, which directory env loaders (e.g. a cloned repo's .envrc) can set.
	env := &runtimeEnv{server: server, serverSource: source, token: token, user: user}
	if token != "" && source != "default" && source != "flag" && !isLocalHost(server.Hostname()) {
		env.token = ""
		env.tokenRefused = fmt.Errorf("refusing to send TUZY_TOKEN to %s (set via %s); pass --server explicitly to allow it", server, source)
	}
	return env, nil
}

func parseServer(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid server URL %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLocalHost(u.Hostname()) {
			return nil, errors.New("server must use https:// unless it is localhost")
		}
	default:
		return nil, fmt.Errorf("unsupported server scheme %q", u.Scheme)
	}
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u, nil
}

func isLocalHost(h string) bool {
	h = strings.ToLower(h)
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasSuffix(h, ".localhost")
}

// userAgent is sent on every edge request, e.g. "tuzy/1.0.0 darwin/arm64".
func userAgent() string {
	return fmt.Sprintf("tuzy/%s %s/%s", strings.TrimPrefix(buildVersion(), "v"), runtime.GOOS, runtime.GOARCH)
}

// displayServer is shown next to "online" when not using the default server.
func (e *runtimeEnv) displayServer() string {
	if e.serverSource == "default" {
		return ""
	}
	return e.server.String()
}
