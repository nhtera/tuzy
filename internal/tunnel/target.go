package tunnel

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ParseTarget turns a user target into a local URL:
//
//	3000                 → http://localhost:3000
//	127.0.0.1:8080       → http://127.0.0.1:8080
//	localhost:5173       → http://localhost:5173
//	http://localhost:5173 → as-is
//
// https:// and file:// targets arrive in a later version (phase 8).
func ParseTarget(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("missing target: use a port (3000), host:port or http://host:port")
	}
	if port, err := strconv.Atoi(s); err == nil {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("invalid port %d", port)
		}
		return &url.URL{Scheme: "http", Host: net.JoinHostPort("localhost", s)}, nil
	}
	if !strings.Contains(s, "://") {
		host, port, err := net.SplitHostPort(s)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("invalid target %q: use a port (3000), host:port or http://host:port", s)
		}
		return &url.URL{Scheme: "http", Host: s}, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid target %q: %w", s, err)
	}
	switch u.Scheme {
	case "http":
	case "https", "file":
		return nil, fmt.Errorf("%s:// targets are not supported yet", u.Scheme)
	default:
		return nil, fmt.Errorf("unsupported target scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("invalid target %q: missing host", s)
	}
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}
