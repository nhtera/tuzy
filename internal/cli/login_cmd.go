package cli

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
	"github.com/nhtera/tuzy/internal/ui"
)

func newLoginCmd() *cobra.Command {
	var email string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in (or sign up) with a code sent to your email",
		Long: `Log in with a 6-digit code sent to your email. The first login creates your account.
The token is stored in your OS keychain. For CI, create a token with ` + "`tuzy tokens create`" + ` and
set TUZY_TOKEN instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if s := env.displayServer(); s != "" {
				fmt.Fprintf(out, "Logging in to %s\n", s)
			}
			p := newPrompter(cmd)
			if email == "" {
				if email, err = p.ask("Email: "); err != nil {
					return err
				}
			}
			client := api.New(env.server, "", userAgent())
			ctx := cmd.Context()
			st, err := client.StartLogin(ctx, email)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "We sent a 6-digit code to %s (valid for %d minutes).\n", email, st.ExpiresIn/60)

			for attempt := 0; attempt < 5; attempt++ {
				code, err := p.ask("Code (press Enter to resend): ")
				if err != nil {
					return err
				}
				if code == "" {
					next, err := client.StartLogin(ctx, email)
					if err != nil {
						// e.g. 429 after a quick double resend: earlier codes still work.
						fmt.Fprintf(out, "Couldn't send another code (%v). Enter a code you already received.\n", err)
						continue
					}
					st = next
					fmt.Fprintln(out, "Sent a new code (earlier codes still work).")
					continue
				}
				token, user, err := client.VerifyLogin(ctx, st.LoginID, normalizeCode(code), deviceLabel())
				if api.IsCode(err, "invalid_code") {
					fmt.Fprintln(out, "That code didn't work. Check the latest email and try again.")
					continue
				}
				if err != nil {
					return err
				}
				store, err := tokenStore(cmd)
				if err != nil {
					return err
				}
				if err := store.Set(env.server.Host, token); err != nil {
					return fmt.Errorf("logged in, but could not save the token: %w", err)
				}
				fmt.Fprintf(out, "%s Logged in as %s\n", ui.Check, user.Email)
				if env.token != "" {
					fmt.Fprintln(out, "note: TUZY_TOKEN is set and takes precedence over this login.")
				}
				return nil
			}
			return errors.New("too many attempts; run `tuzy login` again")
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "your email address")
	return cmd
}

func newLogoutCmd() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Revoke this device's token and close its tunnels",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := resolveEnv(cmd)
			if err != nil {
				return err
			}
			store, err := tokenStore(cmd)
			if err != nil {
				return err
			}
			tok, err := store.Get(env.server.Host)
			if err != nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Not logged in.")
				return nil
			}
			// Revoke server-side first (closes live tunnels); a dead token is fine to forget.
			if !local {
				rerr := api.New(env.server, tok, userAgent()).RevokeToken(cmd.Context(), "current")
				if rerr != nil && !api.IsCode(rerr, "unauthorized") && !api.IsCode(rerr, "token_expired") {
					return fmt.Errorf("could not revoke the token (still stored; `tuzy logout --local` forgets it anyway): %w", rerr)
				}
			}
			if err := store.Delete(env.server.Host); err != nil {
				return err
			}
			if local {
				fmt.Fprintln(cmd.OutOrStdout(), "Forgot the local token (it stays valid on the server until revoked or idle for 90 days).")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Logged out.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "only forget the token on this machine (e.g. when offline)")
	return cmd
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the logged-in account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, env, err := apiClient(cmd)
			if err != nil {
				return err
			}
			me, err := c.Me(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s (%s)\n", me.User.Email, me.User.ID)
			fmt.Fprintf(out, "token:  %s · scope %s\n", me.Token.Label, me.Token.Scope)
			fmt.Fprintf(out, "server: %s\n", env.server)
			if u := me.Usage; u != nil && u.LongStreamBudgetSeconds > 0 {
				fmt.Fprintf(out, "long streams: %s of %s this month (%s)\n",
					hours(u.LongStreamSeconds), hours(u.LongStreamBudgetSeconds), u.Month)
			}
			return nil
		},
	}
}

// hours renders seconds as "12.5 h" (long-stream usage).
func hours(sec int64) string {
	return strconv.FormatFloat(float64(sec)/3600, 'f', 1, 64) + " h"
}
