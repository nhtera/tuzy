package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/names"
	"github.com/nhtera/tuzy/internal/upstream"
)

func newHTTPCmd() *cobra.Command {
	var (
		name             string
		force            bool
		hostHeader       string
		logs             logFlags
		quiet            bool
		inspect          inspectOpts
		upstreamInsecure bool
		upstreamSNI      string
	)
	cmd := &cobra.Command{
		Use:   "http <port | host:port | http://host:port | https://host:port | file:///path>",
		Short: "Expose a local HTTP service at https://<name>.tuzy.dev",
		Long: `Expose a local HTTP service at a permanent https://<name>.tuzy.dev URL.
Without --name your default name is used (the first run asks you to pick one).

The target can be a port, host:port, http://, https:// (use --upstream-insecure for a self-signed
local app) or a file:// directory to serve (read-only, with directory listing; dotfiles hidden).`,
		Example: `  tuzy http 3000
  tuzy http 3000 --name shop
  tuzy http 127.0.0.1:8080 --name api --host-header rewrite
  tuzy http https://localhost:8443 --upstream-insecure
  tuzy http file:///path/to/site`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name = strings.ToLower(name)
			if name != "" && !names.Valid(name) {
				return fmt.Errorf("invalid name %q: 3-32 chars of a-z, 0-9 and single hyphens", name)
			}
			if hostHeader != "preserve" && hostHeader != "rewrite" {
				return errors.New(`--host-header must be "preserve" or "rewrite"`)
			}
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			if env.token, err = resolveToken(cmd, env); err != nil {
				return err
			}
			// Resolve the log destination before buildTarget/runTunnels, so their startup notices
			// (and startInspector's) know whether stdout is reserved for a structured log stream.
			p, lg, notices, err := logs.output(cmd, env, quiet)
			if err != nil {
				return err
			}
			defer func() { _ = lg.Close() }()
			target, display, transport, err := buildCLITarget(args[0], upstream.TLSOptions{SNI: upstreamSNI, Insecure: upstreamInsecure}, notices)
			if err != nil {
				return err
			}
			// Resolve (and, if needed, claim) the name once — never inside the reconnect loop.
			resolved, err := nameResolver(cmd, api.New(env.server, env.token, userAgent())).Resolve(cmd.Context(), name)
			if err != nil {
				return err
			}
			name = resolved
			spec := tunnelSpec{name: name, target: target, display: display, hostHeader: hostHeader, force: force, transport: transport}
			return runTunnels(cmd.Context(), env, []tunnelSpec{spec}, p, lg, inspect, notices)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "tunnel name (default: your default name)")
	cmd.Flags().BoolVar(&force, "force", false, "take over the name even if it is live on another device")
	cmd.Flags().StringVar(&hostHeader, "host-header", "preserve", `Host header sent to the local app: "preserve" (tunnel host) or "rewrite" (local target host)`)
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the access log")
	logs.register(cmd)
	cmd.Flags().BoolVar(&inspect.disabled, "no-inspect", false, "don't start the local request inspector")
	cmd.Flags().StringVar(&inspect.addr, "inspect-addr", "", "inspector address (default 127.0.0.1:4040)")
	cmd.Flags().BoolVar(&upstreamInsecure, "upstream-insecure", false, "skip TLS certificate verification for an https:// upstream (prints a warning)")
	cmd.Flags().StringVar(&upstreamSNI, "upstream-sni", "", "SNI sent to an https:// upstream; it also sets the name verified in the cert")
	return cmd
}
