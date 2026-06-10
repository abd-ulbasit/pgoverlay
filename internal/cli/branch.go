package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

func newBranchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Manage branches"}
	cmd.AddCommand(newBranchCreateCmd(), newBranchLsCmd(), newBranchDestroyCmd())
	return cmd
}

func newBranchCreateCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an instant copy-on-write branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			start := time.Now()
			b, err := e.CreateBranch(cmd.Context(), args[0], from, 0)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q ready in %s (port %d)\n", b.Name, time.Since(start).Round(time.Millisecond), b.Port)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source to branch from")
	cmd.MarkFlagRequired("from")
	return cmd
}

func newBranchLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List branches",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			branches, err := reg.ListLiveBranches()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tPORT\tCREATED")
			for _, b := range branches {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", b.Name, b.State, b.Port, b.CreatedAt)
			}
			return w.Flush()
		},
	}
}

func newBranchDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy NAME",
		Short: "Destroy a branch (container + CoW layer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			if err := e.DestroyBranch(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q destroyed\n", args[0])
			return nil
		},
	}
}

func newConnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect NAME",
		Short: "Print the connection string for a branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			b, err := reg.GetBranchByName(args[0])
			if err != nil {
				return err
			}
			s, err := reg.GetSourceByID(b.SourceID)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@localhost:%d/%s\n", s.ConnUser, b.Port, s.ConnDB)
			return nil
		},
	}
}
