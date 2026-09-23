package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/names"
	"github.com/nhtera/tuzy/internal/tunnel"
	"github.com/nhtera/tuzy/internal/ui"
)

func newHTTPCmd() *cobra.Command {
	var (
		name       string
		force      bool
		hostHeader string
		quiet      bool
		inspect    inspectOpts
	)
	cmd := &cobra.Command{
		Use:   "http <port | host:port | http://host:port>",
		Short: "Expose a local HTTP service at https://<name>.tuzy.dev",
		Long: `Expose a local HTTP service at a permanent https://<name>.tuzy.dev URL.
Without --name your default name is used (the first run asks you to pick one).`,
		Example: `  tuzy http 3000
  tuzy http 3000 --name shop
  tuzy http 127.0.0.1:8080 --name api --host-header rewrite`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := tunnel.ParseTarget(args[0])
			if err != nil {
				return err
			}
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
			// Resolve (and, if needed, claim) the name once — never inside the reconnect loop.
			resolved, err := nameResolver(cmd, api.New(env.server, env.token, userAgent())).Resolve(cmd.Context(), name)
			if err != nil {
				return err
			}
			name = resolved
			p := ui.New(cmd.OutOrStdout(), quiet, env.displayServer())
			return runTunnels(cmd.Context(), env, []tunnelSpec{{name: name, target: target, hostHeader: hostHeader, force: force}}, p, inspect, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "tunnel name (default: your default name)")
	cmd.Flags().BoolVar(&force, "force", false, "take over the name even if it is live on another device")
	cmd.Flags().StringVar(&hostHeader, "host-header", "preserve", `Host header sent to the local app: "preserve" (tunnel host) or "rewrite" (local target host)`)
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the access log")
	cmd.Flags().BoolVar(&inspect.disabled, "no-inspect", false, "don't start the local request inspector")
	cmd.Flags().StringVar(&inspect.addr, "inspect-addr", "", "inspector address (default 127.0.0.1:4040)")
	return cmd
}
