package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/config"
	"github.com/nhtera/tuzy/internal/logging"
	"github.com/nhtera/tuzy/internal/ui"
	"github.com/nhtera/tuzy/internal/upstream"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect and edit tuzy configuration", Args: cobra.NoArgs}
	cmd.AddCommand(newConfigPathCmd(), newConfigCheckCmd(), newConfigEditCmd(), newConfigAddTokenCmd())
	return cmd
}

func userConfigPath(cmd *cobra.Command) (string, error) {
	if p, _ := cmd.Flags().GetString("config"); p != "" {
		return p, nil
	}
	return config.UserPath()
}

func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print where configuration and credentials live",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			user, err := userConfigPath(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "user config:  %s%s\n", user, missing(user))
			proj, _ := filepath.Abs(config.ProjectFile)
			fmt.Fprintf(out, "project file: %s%s\n", proj, missing(proj))
			if s, err := tokenStore(cmd); err == nil {
				fmt.Fprintf(out, "credentials:  OS keychain (service \"tuzy\"), fallback %s%s\n", s.FilePath, missing(s.FilePath))
			}
			return nil
		},
	}
}

func missing(p string) string {
	if _, err := os.Stat(p); err != nil {
		return " (not created)"
	}
	return ""
}

// problems collects config findings for `config check`.
type problems struct{ errs []string }

func (p *problems) add(format string, a ...any) { p.errs = append(p.errs, fmt.Sprintf(format, a...)) }

func newConfigCheckCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate the user config and a tuzy.toml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			var p problems
			userPath, err := userConfigPath(cmd)
			if err != nil {
				return err
			}
			if u, err := config.LoadUser(userPath); err != nil {
				p.add("%v", err)
			} else {
				if u.Server != "" {
					if _, err := parseServer(u.Server); err != nil {
						p.add("%s: server: %v", userPath, err)
					}
				}
				if err := (logging.Options{Dest: u.Log, Format: u.LogFormat, Level: u.LogLevel}).Validate(); err != nil {
					p.add("%s: %v", userPath, err)
				}
				fmt.Fprintf(out, "%s user config %s%s\n", ui.Check, userPath, missing(userPath))
			}
			if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) && !cmd.Flags().Changed("file") {
				fmt.Fprintf(out, "- no %s here\n", config.ProjectFile)
			} else if proj, err := config.LoadProject(file); err != nil {
				p.add("%v", err)
			} else {
				absFile, _ := filepath.Abs(file)
				for _, name := range proj.Names() {
					t := proj.Tunnels[name]
					target, err := upstream.Parse(t.Addr)
					if err != nil {
						p.add("%s: tunnels.%s.addr: %v", file, name, err)
						continue
					}
					if target.Kind == upstream.KindFile {
						if _, err := upstream.ResolveProjectDir(filepath.Dir(absFile), target.Dir); err != nil {
							p.add("%s: tunnels.%s.addr: %v", file, name, err)
							continue
						}
					}
					if (t.UpstreamInsecure || t.UpstreamSNI != "") && target.Kind != upstream.KindHTTPS {
						p.add("%s: tunnels.%s: upstream_insecure/upstream_sni only apply to https:// targets", file, name)
						continue
					}
					if t.UpstreamInsecure {
						fmt.Fprintf(out, "! tunnels.%s: upstream_insecure skips TLS verification of %s\n", name, t.Addr)
					}
				}
				fmt.Fprintf(out, "%s %s: %d tunnel(s) %s\n", ui.Check, file, len(proj.Tunnels), strings.Join(proj.Names(), ", "))
			}
			if len(p.errs) > 0 {
				sort.Strings(p.errs)
				for _, e := range p.errs {
					fmt.Fprintf(out, "%s %s\n", ui.Cross, e)
				}
				return fmt.Errorf("%d problem(s) found", len(p.errs))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", config.ProjectFile, "project file to check")
	return cmd
}

func newConfigEditCmd() *cobra.Command {
	var project bool
	cmd := &cobra.Command{
		Use:   "edit",
		Short: "Open the user config (or --project tuzy.toml) in $EDITOR",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := config.ProjectFile
			if !project {
				var err error
				if path, err = userConfigPath(cmd); err != nil {
					return err
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				if err := os.WriteFile(path, []byte(configTemplate(project)), 0o600); err != nil {
					return err
				}
			}
			editor := os.Getenv("VISUAL")
			if editor == "" {
				editor = os.Getenv("EDITOR")
			}
			if editor == "" {
				editor = "vi"
				if runtime.GOOS == "windows" {
					editor = "notepad"
				}
			}
			parts := strings.Fields(editor)
			c := exec.Command(parts[0], append(parts[1:], path)...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
			return c.Run()
		},
	}
	cmd.Flags().BoolVar(&project, "project", false, "edit ./tuzy.toml instead")
	return cmd
}

func configTemplate(project bool) string {
	if project {
		return `# tuzy.toml — tunnels for ` + "`tuzy start`" + `
# [tunnels.my-app]
# addr = "3000"                 # or "https://localhost:8443", "file://public"
# host_header = "auto"          # or "preserve" / "rewrite"
`
	}
	return `# tuzy user config
# log = "false"            # stdout | stderr | <path> | false
# log_format = "term"      # term | logfmt | json
# log_level = "info"
# inspect_addr = "127.0.0.1:4040"
# update_check = true
`
}

func newConfigAddTokenCmd() *cobra.Command {
	var fileOnly bool
	cmd := &cobra.Command{
		Use:   "add-token <token | ->",
		Short: "Store a token (e.g. a CI or service token) after checking it with the server",
		Long: `Stores a token in the OS keychain (or, with --file, the 0600 credentials file) after checking
it with the server. Pass "-" to read the token from stdin: a token on the command line ends up
in your shell history and is visible to other users in ` + "`ps`" + `.`,
		Example: `  echo "$TUZY_TOKEN" | tuzy config add-token -
  tuzy config add-token - --file < token.txt   # headless service`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := strings.TrimSpace(args[0])
			if token == "-" {
				b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4096))
				if err != nil {
					return err
				}
				token = strings.TrimSpace(string(b))
			}
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			// Same rule as TUZY_TOKEN: never send a pasted token to a server that a directory env
			// loader or config file chose (only the default, --server, or loopback).
			if env.serverSource != "default" && env.serverSource != "flag" && !isLocalHost(env.server.Hostname()) {
				return fmt.Errorf("refusing to send the token to %s (set via %s); pass --server explicitly to allow it", env.server, env.serverSource)
			}
			if args[0] != "-" {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: tokens on the command line end up in shell history; prefer `tuzy config add-token -`")
			}
			me, err := api.New(env.server, token, userAgent()).Me(cmd.Context())
			if err != nil {
				return fmt.Errorf("the server rejected this token: %w", err)
			}
			s, err := tokenStore(cmd)
			if err != nil {
				return err
			}
			if fileOnly {
				err = s.SetFile(env.server.Host, token)
			} else {
				err = s.Set(env.server.Host, token)
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s stored a %s-scope token for %s (%s)\n", ui.Check, me.Token.Scope, me.User.Email, env.displayServer())
			return nil
		},
	}
	cmd.Flags().BoolVar(&fileOnly, "file", false, "store in the 0600 credentials file instead of the OS keychain (headless services)")
	return cmd
}
