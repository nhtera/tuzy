// Package cli wires the tuzy command tree (cobra).
package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build metadata, injected at link time via
// -ldflags "-X github.com/nhtera/tuzy/internal/cli.version=... -X ...commit=... -X ...date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// NewRootCmd builds the root `tuzy` command with all subcommands attached.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "tuzy",
		Short:         "Expose localhost at a stable https://<name>.tuzy.dev URL",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("server", "", "edge server URL (default https://tuzy.dev; env TUZY_SERVER)")
	root.PersistentFlags().String("config", "", "user config file (default $UserConfigDir/tuzy/config.toml)")
	_ = root.PersistentFlags().MarkHidden("server")
	root.AddCommand(newVersionCmd(), newHTTPCmd(), newStartCmd(), newLoginCmd(), newLogoutCmd(), newWhoamiCmd(), newTokensCmd(), newAccountCmd(), newNamesCmd(), newAdminCmd())
	return root
}

// buildVersion returns the ldflags-injected version, falling back to the module version
// recorded by `go install github.com/nhtera/tuzy/cmd/tuzy@vX.Y.Z` (which sets no ldflags).
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

// newVersionCmd prints the build metadata, e.g. "tuzy 1.0.0 (abc1234, 2026-09-24) darwin/arm64".
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tuzy version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "tuzy %s (%s, %s) %s/%s\n",
				buildVersion(), commit, date, runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}
