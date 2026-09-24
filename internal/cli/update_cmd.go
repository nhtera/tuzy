package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"
	"golang.org/x/term"

	"github.com/nhtera/tuzy/internal/ui"
	"github.com/nhtera/tuzy/internal/update"
)

// newUpdater is swapped in tests (fake release server, temp executable).
var newUpdater = func() (*update.Updater, error) { return update.New(buildVersion()) }

func newUpdateCmd() *cobra.Command {
	var check bool
	var target string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update tuzy to the latest release (verified download, atomic replace)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			u, err := newUpdater()
			if err != nil {
				return err
			}
			explicit := target != ""
			version := target
			if explicit {
				if version = canonicalVersion(version); !semver.IsValid(version) {
					return fmt.Errorf("--version %q is not a version like v1.2.3", target)
				}
			} else if version, err = u.Latest(cmd.Context()); err != nil {
				return err
			}
			method := update.DetectMethod(u.Exe, os.Getenv("GOPATH"), os.Getenv("GOBIN"), homeDir())
			if check {
				if u.Newer(version) {
					fmt.Fprintf(out, "tuzy %s is available (you have %s)\n", version, u.Current)
					if h := method.Hint(); h != "" {
						fmt.Fprintf(out, "update with: %s\n", h)
					} else {
						fmt.Fprintln(out, "update with: tuzy update")
					}
				} else {
					fmt.Fprintf(out, "tuzy %s is up to date\n", u.Current)
				}
				return nil
			}
			if h := method.Hint(); h != "" {
				fmt.Fprintf(out, "tuzy was installed with %s; update it with:\n  %s\n", method, h)
				return nil
			}
			if !explicit && !u.Newer(version) {
				fmt.Fprintf(out, "tuzy %s is up to date\n", u.Current)
				return nil
			}
			if explicit && semver.IsValid(u.Current) && semver.Compare(version, u.Current) < 0 {
				fmt.Fprintf(out, "downgrading from %s to %s\n", u.Current, version)
			}
			fmt.Fprintf(out, "downloading tuzy %s…\n", version)
			bin, err := u.Download(cmd.Context(), version)
			if err != nil {
				return err
			}
			if err := u.Install(bin, version); err != nil {
				return err
			}
			fmt.Fprintf(out, "%s updated to %s (%s)\n", ui.Check, version, u.Exe)
			fmt.Fprintln(out, "Running tunnels keep the old version until restarted (`tuzy service restart` for the service).")
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only report whether an update is available")
	cmd.Flags().StringVar(&target, "version", "", "install this version (e.g. v1.2.3, also prereleases or downgrades)")
	return cmd
}

func canonicalVersion(v string) string {
	if v != "" && v[0] != 'v' {
		return "v" + v
	}
	return v
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// updateNotice: refresh the cached latest version in the background and, after the command,
// mention a newer release on stderr at most once a day. Only on a terminal, never in CI, never
// for update/version/service/completion, and not with TUZY_NO_UPDATE_CHECK=1 or update_check = false.
func updateNotice(root *cobra.Command) {
	skip := func(cmd *cobra.Command) bool {
		if os.Getenv("TUZY_NO_UPDATE_CHECK") != "" || os.Getenv("CI") != "" || !term.IsTerminal(int(os.Stderr.Fd())) {
			return true
		}
		for c := cmd; c != nil; c = c.Parent() {
			switch c.Name() {
			case "update", "version", "service", "completion", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
				return true
			}
		}
		if env, err := resolveEnv(cmd); err == nil && env.user.UpdateCheck != nil && !*env.user.UpdateCheck {
			return true // update_check = false in the user config
		}
		return false
	}
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		if runtime.GOOS == "windows" {
			update.CleanupOld(executablePath()) // always, also for services and CI
		}
		if skip(cmd) {
			return
		}
		if u, err := newUpdater(); err == nil {
			go update.RefreshNotice(context.Background(), u, update.NoticePath(), time.Now())
		}
	}
	root.PersistentPostRun = func(cmd *cobra.Command, _ []string) {
		if skip(cmd) {
			return
		}
		if u, err := newUpdater(); err == nil {
			if v := update.PendingNotice(u, update.NoticePath(), time.Now()); v != "" {
				how := update.DetectMethod(u.Exe, os.Getenv("GOPATH"), os.Getenv("GOBIN"), homeDir()).Hint()
				if how == "" {
					how = "tuzy update"
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "\nnote: tuzy %s is available (you have %s): run `%s`\n", v, u.Current, how)
			}
		}
	}
}

func executablePath() string {
	p, _ := os.Executable()
	return p
}
