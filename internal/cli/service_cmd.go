package cli

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/config"
	"github.com/nhtera/tuzy/internal/service"
)

// serviceRunner is swapped in tests.
var serviceRunner service.Runner = service.ExecRunner

func newServiceCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Run `tuzy start --all` as a background service at login",
		Long: `Installs a user-level service that runs every tunnel in a tuzy.toml at login and keeps it up:
a launchd agent on macOS, a systemd --user unit on Linux, a logon task on Windows.
Logs are JSON; ` + "`tuzy service status`" + ` prints where.`,
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVarP(&file, "file", "f", config.ProjectFile, "project file with the tunnels to run")

	manager := func() (service.Manager, error) {
		// A LaunchAgent lives in the login user's gui/<uid> domain; under sudo that would be gui/0,
		// which doesn't exist ("125: Domain does not support specified action").
		if runtime.GOOS == "darwin" && os.Geteuid() == 0 {
			return service.Manager{}, fmt.Errorf("tuzy service is a per-user LaunchAgent: run it without sudo")
		}
		exe, err := os.Executable()
		if err != nil {
			return service.Manager{}, err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return service.Manager{}, err
		}
		exe = service.StableExe(exe) // survive brew upgrade / scoop update
		proj, err := filepath.Abs(file)
		if err != nil {
			return service.Manager{}, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return service.Manager{}, err
		}
		spec := service.Spec{GOOS: runtime.GOOS, Exe: exe, ProjectFile: proj, LogPath: service.DefaultLogPath(runtime.GOOS, home), Home: home, UID: os.Getuid()}
		if u, err := user.Current(); err == nil {
			spec.User = u.Username
		}
		// The service must talk to the same server with the same user config as this command.
		if env, err := resolveEnv(cmd); err == nil && (env.serverSource == "flag" || env.serverSource == "env") {
			spec.ExtraArgs = append(spec.ExtraArgs, "--server", env.server.String())
		}
		if c, _ := cmd.Flags().GetString("config"); c != "" {
			if abs, err := filepath.Abs(c); err == nil {
				spec.ExtraArgs = append(spec.ExtraArgs, "--config", abs)
			}
		}
		return service.Manager{Spec: spec, Run: serviceRunner}, nil
	}
	action := func(use, short string, fn func(cmd *cobra.Command, m service.Manager) error) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := manager()
			if err != nil {
				return err
			}
			return fn(cmd, m)
		}}
	}

	var dryRun bool
	install := action("install", "Install the service (then `tuzy service start`)", func(cmd *cobra.Command, m service.Manager) error {
		out := cmd.OutOrStdout()
		if dryRun {
			content, err := m.Spec.Render()
			if err != nil {
				return err
			}
			if p := m.Spec.UnitPath(); p != "" {
				fmt.Fprintf(out, "# %s\n", p)
			} else {
				fmt.Fprintln(out, "# schtasks /Create /SC ONLOGON /TN tuzy /RL LIMITED /TR:")
			}
			fmt.Fprintln(out, content)
			return nil
		}
		if _, err := config.LoadProject(m.Spec.ProjectFile); err != nil {
			return fmt.Errorf("%s: %w", m.Spec.ProjectFile, err)
		}
		// The service can't prompt and doesn't inherit TUZY_TOKEN: a token must be STORED for
		// this server (keychain or credentials file).
		env, err := resolveEnv(cmd)
		if err != nil {
			return err
		}
		store, err := tokenStore(cmd)
		if err != nil {
			return err
		}
		if _, err := store.Get(env.server.Host); err != nil {
			return fmt.Errorf("the service needs a stored token for %s: run `tuzy login` or `tuzy config add-token` first (%w)", env.server.Host, err)
		}
		if err := m.Install(); err != nil {
			return err
		}
		fmt.Fprintf(out, "Installed: runs `tuzy %s` at login.\nLogs: %s\nStart it now with `tuzy service start`.\n", strings.Join(m.Spec.Args(), " "), m.Spec.LogPath)
		if runtime.GOOS == "linux" {
			fmt.Fprintln(out, "To keep tunnels up after you log out: loginctl enable-linger $USER")
		}
		return nil
	})
	install.Flags().BoolVar(&dryRun, "dry-run", false, "print the unit/plist instead of installing")

	cmd.AddCommand(
		install,
		action("uninstall", "Stop and remove the service", func(cmd *cobra.Command, m service.Manager) error {
			if err := m.Uninstall(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Service removed.")
			return nil
		}),
		action("start", "Start the service", func(cmd *cobra.Command, m service.Manager) error { return done(cmd, m.Start(), "started") }),
		action("stop", "Stop the service (it starts again at next login)", func(cmd *cobra.Command, m service.Manager) error { return done(cmd, m.Stop(), "stopped") }),
		action("restart", "Restart the service (after `tuzy update` or editing tuzy.toml)", func(cmd *cobra.Command, m service.Manager) error {
			return done(cmd, m.Restart(), "restarted")
		}),
		action("status", "Show whether the service runs and where it logs", func(cmd *cobra.Command, m service.Manager) error {
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s\nlogs:   %s\n", m.Status(), m.Spec.LogPath)
			return nil
		}),
	)
	return cmd
}

func done(cmd *cobra.Command, err error, what string) error {
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Service %s.\n", what)
	return nil
}
