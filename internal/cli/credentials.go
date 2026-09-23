package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/auth"
)

// tokenStore is swappable in tests.
var tokenStore = func(cmd *cobra.Command) (*auth.Store, error) {
	s, err := auth.DefaultStore()
	if err != nil {
		return nil, err
	}
	s.Notice = func(m string) { fmt.Fprintln(cmd.ErrOrStderr(), "note:", m) }
	return s, nil
}

// resolveToken: TUZY_TOKEN (CI) → OS keychain → credentials file.
func resolveToken(cmd *cobra.Command, env *runtimeEnv) (string, error) {
	if env.tokenRefused != nil {
		return "", env.tokenRefused
	}
	if env.token != "" {
		return env.token, nil
	}
	s, err := tokenStore(cmd)
	if err != nil {
		return "", err
	}
	return s.Get(env.server.Host)
}

// apiClient returns an authenticated API client (or ErrNoToken).
func apiClient(cmd *cobra.Command) (*api.Client, *runtimeEnv, error) {
	env, err := resolveEnv(cmd)
	if err != nil {
		return nil, nil, err
	}
	tok, err := resolveToken(cmd, env)
	if err != nil {
		return nil, nil, err
	}
	env.token = tok
	return api.New(env.server, tok, userAgent()), env, nil
}

// deviceLabel names a login token after this machine, e.g. "tien-mbp (darwin)".
func deviceLabel() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "cli"
	}
	host = strings.TrimSuffix(strings.TrimSuffix(host, ".local"), ".lan")
	return fmt.Sprintf("%s (%s)", host, runtime.GOOS)
}

// prompter reads answers from the command's stdin.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func newPrompter(cmd *cobra.Command) *prompter {
	return &prompter{in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout()}
}

var errNoInput = errors.New("no input (stdin closed)")

// normalizeCode accepts "482913", "482 913" or "482-913".
func normalizeCode(s string) string {
	return strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(s))
}

func (p *prompter) ask(question string) (string, error) {
	fmt.Fprint(p.out, question)
	line, err := p.in.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		if errors.Is(err, io.EOF) {
			return "", errNoInput
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}
