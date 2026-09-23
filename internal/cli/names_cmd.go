package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/config"
)

func newNamesCmd() *cobra.Command {
	ls := newNamesLsCmd()
	cmd := &cobra.Command{
		Use:   "names",
		Short: "Manage your permanent subdomains (up to 10)",
		Args:  cobra.NoArgs,
		RunE:  ls.RunE,
	}
	cmd.Flags().AddFlagSet(ls.Flags())
	cmd.AddCommand(ls, newNamesAddCmd(), newNamesRenameCmd(), newNamesDefaultCmd(), newNamesRmCmd())
	return cmd
}

func lastSeen(ts *int64) string {
	if ts == nil {
		return "never"
	}
	d := time.Since(time.Unix(*ts, 0))
	if d < 2*time.Minute {
		return "now"
	}
	return time.Unix(*ts, 0).Format("2006-01-02 15:04")
}

func newNamesLsCmd() *cobra.Command {
	var asJSON, live bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List your names",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			list, err := c.ListNames(cmd.Context(), live)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(list)
			}
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tURL\tSTATE\tSTATUS\tLAST SEEN\tDEFAULT")
			for _, n := range list.Names {
				state := n.State
				if state == "" {
					state = "-"
				}
				def := ""
				if n.Default {
					def = "*"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", n.Name, n.URL, strings.ReplaceAll(state, "_", " "), n.Status, lastSeen(n.LastSeenAt), def)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "%d/%d names used\n", list.Used, list.Limit)
			for _, h := range list.Held {
				note := "released"
				if h.RenamedTo != nil {
					note = "renamed to " + *h.RenamedTo
				}
				fmt.Fprintf(out, "on hold for you until %s: %s (%s) — reclaim with `tuzy names add %s`\n",
					time.Unix(h.Until, 0).Format("2006-01-02"), h.Name, note, h.Name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&live, "live", true, "show whether each tunnel is online")
	return cmd
}

func newNamesAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [name]",
		Short: "Reserve a name (no name: pick a random one); also reclaims a name you released",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = strings.ToLower(args[0])
			}
			n, err := c.AddName(cmd.Context(), name)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Reserved %s → %s\n", n.Name, n.URL)
			return nil
		},
	}
}

// confirm asks unless --yes was given; without a terminal it refuses rather than guess.
func confirm(cmd *cobra.Command, yes bool, question string) error {
	if yes {
		return nil
	}
	if !isInteractive(cmd) {
		return errors.New("not a terminal: pass --yes to confirm")
	}
	answer, err := newPrompter(cmd).ask(question + " [y/N]: ")
	if err != nil {
		return err
	}
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		return errors.New("aborted")
	}
	return nil
}

func newNamesRenameCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a name (a running tunnel moves automatically)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldName, newName := strings.ToLower(args[0]), strings.ToLower(args[1])
			if err := confirm(cmd, yes, fmt.Sprintf("Webhooks pointing at %s will break; %s stays reserved for you for 12 months. Rename?", oldName, oldName)); err != nil {
				return err
			}
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			n, err := c.RenameName(cmd.Context(), oldName, newName)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Renamed %s → %s (%s)\n", oldName, n.Name, n.URL)
			if proj, err := config.LoadProject(config.ProjectFile); err == nil {
				if _, ok := proj.Tunnels[oldName]; ok {
					fmt.Fprintf(out, "note: %s still lists [tunnels.%s]; rename it to [tunnels.%s]\n", config.ProjectFile, oldName, n.Name)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(out, "note: could not read %s: %v\n", config.ProjectFile, err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

func newNamesDefaultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "default <name>",
		Short: "Use this name when `tuzy http` has no --name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			n, err := c.SetDefaultName(cmd.Context(), strings.ToLower(args[0]))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Default name is now %s\n", n.Name)
			return nil
		},
	}
}

func newNamesRmCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Release a name (held for you for 12 months)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.ToLower(args[0])
			if err := confirm(cmd, yes, fmt.Sprintf("Webhooks pointing at %s will break; %s stays reserved for you for 12 months. Remove?", name, name)); err != nil {
				return err
			}
			c, _, err := apiClient(cmd)
			if err != nil {
				return err
			}
			if err := c.RemoveName(cmd.Context(), name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s (reclaim within 12 months with `tuzy names add %s`)\n", name, name)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}
