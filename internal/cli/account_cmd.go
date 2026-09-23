package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/api"
)

func newAccountCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Manage your account"}
	cmd.AddCommand(&cobra.Command{
		Use:   "delete",
		Short: "Permanently delete your account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, env, err := apiClient(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			me, err := c.Me(ctx)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, `This permanently deletes %s:
  • every token is revoked and every tunnel closes now
  • your names stay reserved for you for 12 months, then are released
  • your email is erased after 30 days
`, me.User.Email)
			p := newPrompter(cmd)
			typed, err := p.ask("Type your email to confirm: ")
			if err != nil {
				return err
			}
			if !strings.EqualFold(typed, me.User.Email) {
				return errors.New("email did not match; nothing was deleted")
			}
			st, err := c.StartAccountDeletion(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "We sent a confirmation code to %s.\n", me.User.Email)
			for attempt := 0; attempt < 5; attempt++ {
				code, err := p.ask("Code: ")
				if err != nil {
					return err
				}
				err = c.ConfirmAccountDeletion(ctx, st.LoginID, normalizeCode(code))
				if api.IsCode(err, "invalid_code") {
					fmt.Fprintln(out, "That code didn't work. Try again.")
					continue
				}
				if err != nil {
					return err
				}
				if store, serr := tokenStore(cmd); serr == nil {
					_ = store.Delete(env.server.Host)
				}
				fmt.Fprintln(out, "Account deleted.")
				return nil
			}
			return errors.New("too many attempts; nothing was deleted")
		},
	})
	return cmd
}
