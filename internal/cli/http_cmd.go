package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

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
	)
	cmd := &cobra.Command{
		Use:   "http <port | host:port | http://host:port>",
		Short: "Expose a local HTTP service at https://<name>.tuzy.dev",
		Example: `  tuzy http 3000 --name shop
  tuzy http 127.0.0.1:8080 --name api --host-header rewrite`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := tunnel.ParseTarget(args[0])
			if err != nil {
				return err
			}
			if name == "" {
				return errors.New("--name is required for now (pinned names arrive with `tuzy login`)")
			}
			if !names.Valid(name) {
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
			p := ui.New(cmd.OutOrStdout(), quiet, env.displayServer())
			return runTunnels(cmd.Context(), env, []tunnelSpec{{name: name, target: target, hostHeader: hostHeader, force: force}}, p)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "tunnel name (subdomain)")
	cmd.Flags().BoolVar(&force, "force", false, "take over the name even if it is live on another device")
	cmd.Flags().StringVar(&hostHeader, "host-header", "preserve", `Host header sent to the local app: "preserve" (tunnel host) or "rewrite" (local target host)`)
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the access log")
	return cmd
}
