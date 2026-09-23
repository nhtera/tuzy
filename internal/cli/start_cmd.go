package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/config"
	"github.com/nhtera/tuzy/internal/tunnel"
	"github.com/nhtera/tuzy/internal/ui"
)

func newStartCmd() *cobra.Command {
	var (
		all   bool
		file  string
		quiet bool
		force bool
	)
	cmd := &cobra.Command{
		Use:   "start [name…] | --all",
		Short: "Start tunnels defined in tuzy.toml",
		Example: `  # tuzy.toml
  [tunnels.web]
  addr = "3000"

  [tunnels.api]
  addr = "8080"
  host_header = "rewrite"

  tuzy start --all
  tuzy start web`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("name the tunnels to start, or use --all")
			}
			proj, err := config.LoadProject(file)
			if err != nil {
				return err
			}
			selected := args
			if all {
				selected = proj.Names()
			}
			if len(selected) == 0 {
				return fmt.Errorf("%s defines no tunnels", file)
			}
			var specs []tunnelSpec
			for _, n := range selected {
				t, ok := proj.Tunnels[n]
				if !ok {
					return fmt.Errorf("tunnel %q is not defined in %s", n, file)
				}
				target, err := tunnel.ParseTarget(t.Addr)
				if err != nil {
					return fmt.Errorf("tunnels.%s: %w", n, err)
				}
				hh := t.HostHeader
				if hh == "" {
					hh = "preserve"
				}
				specs = append(specs, tunnelSpec{name: n, target: target, hostHeader: hh, force: force})
			}
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			if env.token, err = resolveToken(cmd, env); err != nil {
				return err
			}
			// Pre-check every name once (owned / claim if free / refuse holds) before connecting.
			resolver := nameResolver(cmd, api.New(env.server, env.token, userAgent()))
			for _, s := range specs {
				if _, err := resolver.Resolve(cmd.Context(), s.name); err != nil {
					return fmt.Errorf("tunnels.%s: %w", s.name, err)
				}
			}
			return runTunnels(cmd.Context(), env, specs, ui.New(cmd.OutOrStdout(), quiet, env.displayServer()))
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "start every tunnel in the file")
	cmd.Flags().StringVarP(&file, "file", "f", config.ProjectFile, "project file")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the access log")
	cmd.Flags().BoolVar(&force, "force", false, "take over names that are live on another device")
	return cmd
}
