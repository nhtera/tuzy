package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newTokensCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tokens", Short: "Manage API tokens (e.g. for CI)"}
	cmd.AddCommand(newTokensLsCmd(), newTokensCreateCmd(), newTokensRmCmd())
	return cmd
}

func ago(ts *int64) string {
	if ts == nil {
		return "never"
	}
	return time.Unix(*ts, 0).Format("2006-01-02")
}

func newTokensLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List your active tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			toks, err := c.ListTokens(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tLABEL\tSCOPE\tCREATED\tLAST USED\tEXPIRES")
			for _, t := range toks {
				id := t.ID
				if t.Current {
					id += " *"
				}
				created := t.CreatedAt
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", id, t.Label, t.Scope, ago(&created), ago(t.LastUsedAt), ago(t.ExpiresAt))
			}
			return w.Flush()
		},
	}
}

// parseDays accepts "90d" or "90".
func parseDays(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(s), "d"))
	if err != nil || n < 1 || n > 3650 {
		return 0, fmt.Errorf("invalid --expires %q: use days, e.g. 90d", s)
	}
	return n, nil
}

func newTokensCreateCmd() *cobra.Command {
	var label, scope, expires string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a token (default scope: connect, for CI)",
		Example: `  tuzy tokens create --label github-actions --expires 90d
  TUZY_TOKEN=<token> tuzy http 3000 --name preview`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if scope != "connect" && scope != "full" {
				return errors.New(`--scope must be "connect" or "full"`)
			}
			days := 0
			if expires != "" {
				var err error
				if days, err = parseDays(expires); err != nil {
					return err
				}
			}
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			t, err := c.CreateToken(cmd.Context(), label, scope, days)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created %s (%s, scope %s). Copy it now, it won't be shown again:\n\n  %s\n\n", t.ID, t.Label, t.Scope, t.Token)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "ci", "label shown in `tuzy tokens ls`")
	cmd.Flags().StringVar(&scope, "scope", "connect", `"connect" (open tunnels only) or "full"`)
	cmd.Flags().StringVar(&expires, "expires", "", "absolute expiry in days, e.g. 90d (tokens also expire after 90 idle days)")
	return cmd
}

func newTokensRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id>",
		Short: "Revoke a token and close its tunnels",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			if err := c.RevokeToken(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s.\n", args[0])
			return nil
		},
	}
}
