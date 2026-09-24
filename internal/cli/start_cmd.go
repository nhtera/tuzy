package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/config"
)

func newStartCmd() *cobra.Command {
	var (
		all     bool
		file    string
		logs    logFlags
		quiet   bool
		force   bool
		inspect inspectOpts
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
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			if env.token, err = resolveToken(cmd, env); err != nil {
				return err
			}
			// Resolve the log destination before buildProjectTarget/runTunnels, so their startup
			// notices (and startInspector's) know whether stdout is reserved for a structured log
			// stream.
			p, lg, notices, err := logs.output(cmd, env, quiet)
			if err != nil {
				return err
			}
			defer func() { _ = lg.Close() }()
			var specs []tunnelSpec
			for _, n := range selected {
				t, ok := proj.Tunnels[n]
				if !ok {
					return fmt.Errorf("tunnel %q is not defined in %s", n, file)
				}
				target, display, transport, err := buildProjectTarget(file, t.Addr, tunnelTLS(t), notices)
				if err != nil {
					return fmt.Errorf("tunnels.%s: %w", n, err)
				}
				defer closeTransport(transport) // also on early errors before runTunnels
				hh := t.HostHeader
				if hh == "" {
					hh = "auto"
				}
				specs = append(specs, tunnelSpec{name: n, target: target, display: display, hostHeader: hh, force: force, transport: transport})
			}
			// Pre-check every name once (owned / claim if free / refuse holds) before connecting.
			resolver := nameResolver(cmd, api.New(env.server, env.token, userAgent()))
			for _, s := range specs {
				if _, err := resolver.Resolve(cmd.Context(), s.name); err != nil {
					return fmt.Errorf("tunnels.%s: %w", s.name, err)
				}
			}
			return runTunnels(cmd.Context(), env, specs, p, lg, inspect, notices)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "start every tunnel in the file")
	cmd.Flags().StringVarP(&file, "file", "f", config.ProjectFile, "project file")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the access log")
	logs.register(cmd)
	cmd.Flags().BoolVar(&force, "force", false, "take over names that are live in another tuzy process or on another device")
	cmd.Flags().BoolVar(&inspect.disabled, "no-inspect", false, "don't start the local request inspector")
	cmd.Flags().StringVar(&inspect.addr, "inspect-addr", "", "inspector address (default 127.0.0.1:4040)")
	return cmd
}
