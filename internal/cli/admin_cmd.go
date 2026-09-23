package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/ui"
)

// newAdminCmd is the hidden operator toolbox (role=admin). Bootstrap the first admin with:
//
//	wrangler d1 execute tuzy --remote --command "UPDATE users SET role='admin' WHERE email='…'"
func newAdminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "admin",
		Short:  "Operator commands (admin accounts only)",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(newAdminReportsCmd(), newAdminReportCmd(), newAdminOutboxCmd(), newAdminReserveCmd(), newAdminMaxNamesCmd(), newAdminAuditCmd())
	for _, op := range []struct{ use, path, short string }{
		{"suspend-user", "/users/%s/suspend", "Suspend an account (closes its tunnels)"},
		{"unsuspend-user", "/users/%s/unsuspend", "Reinstate an account"},
		{"trust-user", "/users/%s/trust", "Skip the browser interstitial for an account"},
		{"untrust-user", "/users/%s/untrust", "Show the interstitial again (sticky; disables auto-trust)"},
		{"suspend-name", "/names/%s/suspend", "Suspend a name (visitors get 451)"},
		{"unsuspend-name", "/names/%s/unsuspend", "Reinstate a name"},
		{"block-name", "/names/%s/block", "Block a name for everyone (removes a current reservation)"},
		{"release-name", "/names/%s/release", "Lift any hold or block on a name"},
	} {
		cmd.AddCommand(newAdminActionCmd(op.use, op.path, op.short))
	}
	return cmd
}

func printJSON(cmd *cobra.Command, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), buf.String())
	return nil
}

func newAdminActionCmd(use, pathFmt, short string) *cobra.Command {
	var reason string
	arg := "<user-id|email>"
	if strings.HasPrefix(pathFmt, "/names/") {
		arg = "<name>"
	}
	cmd := &cobra.Command{
		Use:   use + " " + arg,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if reason != "" {
				body["reason"] = reason
			}
			out, err := c.AdminPost(cmd.Context(), fmt.Sprintf(pathFmt, url.PathEscape(strings.ToLower(args[0]))), body)
			if err != nil {
				return err
			}
			return printJSON(cmd, out)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "recorded in the audit log")
	return cmd
}

func newAdminReportsCmd() *cobra.Command {
	var status string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "reports",
		Short: "List abuse reports",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			reports, err := c.AdminReports(cmd.Context(), status)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(reports)
			}
			if len(reports) == 0 {
				fmt.Fprintln(out, "no reports")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tWHEN\tNAME\tNAME STATUS\tCATEGORY\tSTATUS\tALERTED\tREPORTER\tDETAILS")
			for _, r := range reports {
				reporter := r.ReporterPrefix
				if r.ReporterLoggedIn == 1 {
					reporter += " (user)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ID, time.Unix(r.CreatedAt, 0).Format("01-02 15:04"), r.Name, deref(r.NameStatus, "gone"), r.Category, r.Status,
					yesNo(r.AlertSent == 1), ui.Sanitize(reporter), ui.Sanitize(truncate(deref(r.Details, ""), 60)))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "open", "open | actioned | dismissed | all")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func newAdminReportCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "report <id> actioned|dismissed",
		Short:     "Close a report (actioned also revokes the owner's auto-trust)",
		Args:      cobra.ExactArgs(2),
		ValidArgs: []string{"actioned", "dismissed"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[1] != "actioned" && args[1] != "dismissed" {
				return fmt.Errorf("status must be actioned or dismissed, not %q", args[1])
			}
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.AdminPost(cmd.Context(), "/reports/"+url.PathEscape(args[0]), map[string]any{"status": args[1]})
			if err != nil {
				return err
			}
			return printJSON(cmd, out)
		},
	}
}

func newAdminReserveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reserve <user-id|email> <name>",
		Short: "Reserve a name for a user (ignores quota, holds and the brand blocklist)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.AdminPost(cmd.Context(), "/names/"+url.PathEscape(strings.ToLower(args[1]))+"/reserve", map[string]any{"user": args[0]})
			if err != nil {
				return err
			}
			return printJSON(cmd, out)
		},
	}
}

func newAdminMaxNamesCmd() *cobra.Command {
	var newAccounts bool
	cmd := &cobra.Command{
		Use:   "set-max-names <user-id|email> <n> | --new-accounts <n>",
		Short: "Change an account's name quota, or the quota of accounts created from now on",
		Args: func(_ *cobra.Command, args []string) error {
			if newAccounts {
				return cobra.ExactArgs(1)(nil, args)
			}
			return cobra.ExactArgs(2)(nil, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			n, err := strconv.Atoi(args[len(args)-1])
			if err != nil {
				return fmt.Errorf("n must be a number: %w", err)
			}
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			path := "/settings/new-account-max-names"
			if !newAccounts {
				path = "/users/" + url.PathEscape(args[0]) + "/max-names"
			}
			out, err := c.AdminPost(cmd.Context(), path, map[string]any{"max": n})
			if err != nil {
				return err
			}
			return printJSON(cmd, out)
		},
	}
	cmd.Flags().BoolVar(&newAccounts, "new-accounts", false, "set the default quota for new accounts")
	return cmd
}

func newAdminAuditCmd() *cobra.Command {
	var target, actor, action string
	var limit int
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show the audit log (newest first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			q := url.Values{"limit": {strconv.Itoa(limit)}}
			for k, v := range map[string]string{"target": target, "actor": actor, "action": action} {
				if v != "" {
					q.Set(k, v)
				}
			}
			out, err := c.AdminGet(cmd.Context(), "/audit?"+q.Encode())
			if err != nil {
				return err
			}
			return printJSON(cmd, out)
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "a name, user id or report id")
	cmd.Flags().StringVar(&actor, "actor", "", "user id of the actor")
	cmd.Flags().StringVar(&action, "action", "", "action prefix, e.g. name.unsuspend")
	cmd.Flags().IntVar(&limit, "limit", 100, "max entries (≤ 500)")
	return cmd
}

func newAdminOutboxCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "outbox",
		Short: "Show pending D1 → tunnel pushes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.AdminGet(cmd.Context(), "/outbox")
			if err != nil {
				return err
			}
			return printJSON(cmd, out)
		},
	}
}

func deref(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func truncate(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
