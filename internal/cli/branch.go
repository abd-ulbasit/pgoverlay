package cli

import (
	"fmt"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/api"
)

func newBranchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Manage branches"}
	cmd.AddCommand(newBranchCreateCmd(), newBranchLsCmd(), newBranchDestroyCmd(), newBranchResetCmd())
	return cmd
}

func newBranchCreateCmd() *cobra.Command {
	var from string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an instant copy-on-write branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			if c := serverClient(cmd); c != nil {
				b, err := c.CreateBranch(cmd.Context(), api.CreateBranchRequest{
					Name: args[0], Source: from, TTLSeconds: int(ttl / time.Second)})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "branch %q ready in %s (port %d, proxy db %q)\n",
					b.Name, time.Since(start).Round(time.Millisecond), b.Port, b.ProxyDatabase)
				return nil
			}
			e, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			b, err := e.CreateBranch(cmd.Context(), args[0], from, ttl)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q ready in %s (port %d)\n", b.Name, time.Since(start).Round(time.Millisecond), b.Port)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source to branch from")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "auto-destroy after this duration (e.g. 24h); 0 = never")
	cmd.MarkFlagRequired("from")
	return cmd
}

func newBranchLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List branches",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSTATE\tPORT\tEXPIRES\tCREATED")
			if c := serverClient(cmd); c != nil {
				branches, err := c.ListBranches(cmd.Context())
				if err != nil {
					return err
				}
				for _, b := range branches {
					fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", b.Name, b.State, b.Port, orNever(b.ExpiresAt), b.CreatedAt)
				}
				return w.Flush()
			}
			_, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			branches, err := reg.ListLiveBranches()
			if err != nil {
				return err
			}
			for _, b := range branches {
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", b.Name, b.State, b.Port, orNever(b.ExpiresAt), b.CreatedAt)
			}
			return w.Flush()
		},
	}
}

func orNever(expiresAt string) string {
	if expiresAt == "" {
		return "never"
	}
	return expiresAt
}

func newBranchDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy NAME",
		Short: "Destroy a branch (container + CoW layer)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := serverClient(cmd); c != nil {
				if err := c.DestroyBranch(cmd.Context(), args[0]); err != nil {
					return err
				}
			} else {
				e, reg, err := open()
				if err != nil {
					return err
				}
				defer reg.Close()
				if err := e.DestroyBranch(cmd.Context(), args[0]); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q destroyed\n", args[0])
			return nil
		},
	}
}

func newBranchResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset NAME",
		Short: "Discard a branch's writes and re-clone it from its source snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := time.Now()
			port := 0
			if c := serverClient(cmd); c != nil {
				b, err := c.ResetBranch(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				port = b.Port
			} else {
				e, reg, err := open()
				if err != nil {
					return err
				}
				defer reg.Close()
				b, err := e.ResetBranch(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				port = b.Port
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q reset in %s (new port %d)\n",
				args[0], time.Since(start).Round(time.Millisecond), port)
			return nil
		},
	}
}

func newConnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connect NAME",
		Short: "Print connection strings for a branch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := serverClient(cmd); c != nil {
				b, err := c.GetBranch(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				u, err := url.Parse(c.BaseURL)
				if err != nil {
					return err
				}
				serverHost := u.Hostname()
				// direct URL targets the branch's recorded host (pod IP on
				// k8s); pre-v3 servers send no host, fall back to the server's
				directHost := b.Host
				if directHost == "" {
					directHost = serverHost
				}
				fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@%s:%d/%s\n", b.User, directHost, b.Port, b.Database)
				fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@%s:6432/%s\n", b.User, serverHost, b.ProxyDatabase)
				return nil
			}
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
			fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@%s:%d/%s\n", s.ConnUser, b.Host, b.Port, s.ConnDB)
			return nil
		},
	}
}
