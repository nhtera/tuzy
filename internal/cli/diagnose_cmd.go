package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/diagnose"
	"github.com/nhtera/tuzy/internal/protocol"
	"github.com/nhtera/tuzy/internal/ui"
)

// errDiagnoseFailed makes `tuzy diagnose` exit 1 without repeating the report.
var errDiagnoseFailed = errors.New("some checks failed")

func newDiagnoseCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Check connectivity to tuzy (DNS, TLS, proxy, WebSocket, token, clock)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			tok, tokErr := resolveToken(cmd, env)
			inspect := env.user.InspectAddr
			results := diagnose.Run(cmd.Context(), diagnose.Config{
				Server: env.server, Token: tok, TokenErr: tokErr, UserAgent: userAgent(),
				Proto: protocol.Version, InspectAddr: inspect,
			})
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(map[string]any{"server": env.server.String(), "ok": !diagnose.Failed(results), "checks": results}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(out, "tuzy diagnose · %s\n\n", env.server)
				w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				for _, r := range results {
					fmt.Fprintf(w, "%s\t%s\t%s\n", mark(r.Status), r.Name, ui.Sanitize(r.Detail))
					if r.Hint != "" && r.Status != diagnose.OK {
						fmt.Fprintf(w, "\t\t→ %s\n", r.Hint)
					}
				}
				if err := w.Flush(); err != nil {
					return err
				}
			}
			if diagnose.Failed(results) {
				cmd.SilenceUsage = true
				return errDiagnoseFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func mark(s diagnose.Status) string {
	switch s {
	case diagnose.OK:
		return ui.Check
	case diagnose.Warn:
		return "!"
	case diagnose.Fail:
		return ui.Cross
	}
	return "-"
}
