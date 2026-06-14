package cli

import (
	"fmt"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func newBranchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Manage branches"}
	cmd.AddCommand(newBranchCreateCmd(), newBranchLsCmd(), newBranchDestroyCmd(), newBranchResetCmd())
	return cmd
}

func newBranchCreateCmd() *cobra.Command {
	var from, fromBranch string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an instant copy-on-write branch (off a source, or off another branch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (from == "") == (fromBranch == "") {
				return fmt.Errorf("exactly one of --from (source) or --from-branch (parent branch) is required")
			}
			start := time.Now()
			if c := serverClient(cmd); c != nil {
				b, err := c.CreateBranch(cmd.Context(), api.CreateBranchRequest{
					Name: args[0], Source: from, Parent: fromBranch, TTLSeconds: int(ttl / time.Second)})
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
			create := func() (*registry.Branch, error) { return e.CreateBranch(cmd.Context(), args[0], from, ttl) }
			if fromBranch != "" {
				create = func() (*registry.Branch, error) { return e.CreateBranchFrom(cmd.Context(), args[0], fromBranch, ttl) }
			}
			b, err := create()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q ready in %s (port %d)\n", b.Name, time.Since(start).Round(time.Millisecond), b.Port)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "source to branch from")
	cmd.Flags().StringVar(&fromBranch, "from-branch", "", "existing branch to branch from (branch-from-branch)")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "auto-destroy after this duration (e.g. 24h); 0 = never")
	return cmd
}

func newBranchLsCmd() *cobra.Command {
	var withUsage bool
	cmd := &cobra.Command{
		Use: "ls", Short: "List branches",
		RunE: func(cmd *cobra.Command, args []string) error {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			header := "NAME\tPARENT\tSTATE\tPORT\tEXPIRES\tCREATED"
			if withUsage {
				header += "\tSIZE"
			}
			fmt.Fprintln(w, header)
			row := func(name, parent, state string, port int, expiresAt, createdAt string, usage func() (int64, error)) {
				if parent == "" {
					parent = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s", name, parent, state, port, orNever(expiresAt), createdAt)
				if withUsage {
					if n, err := usage(); err != nil {
						fmt.Fprintf(w, "\t? (%v)", err)
					} else {
						fmt.Fprintf(w, "\t%s", humanBytes(n))
					}
				}
				fmt.Fprintln(w)
			}
			if c := serverClient(cmd); c != nil {
				branches, err := c.ListBranches(cmd.Context())
				if err != nil {
					return err
				}
				for _, b := range branches {
					row(b.Name, b.Parent, b.State, b.Port, b.ExpiresAt, b.CreatedAt,
						func() (int64, error) { return c.BranchUsage(cmd.Context(), b.Name) })
				}
				return w.Flush()
			}
			e, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			branches, err := reg.ListLiveBranches()
			if err != nil {
				return err
			}
			for _, b := range branches {
				row(b.Name, b.ParentBranchName, string(b.State), b.Port, b.ExpiresAt, b.CreatedAt,
					func() (int64, error) { return e.BranchUsage(cmd.Context(), b.Name) })
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVar(&withUsage, "usage", false, "measure each branch's rw-layer disk usage (runs one helper container per branch)")
	return cmd
}

// humanBytes renders n in binary units (B, KiB, MiB, ...).
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
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

// newHistoryCmd prints a branch's audit trail: every recorded state transition
// with its reason, the actor that caused it (token name + role, the env-token
// sentinel, or system:reconcile for daemon-initiated changes), and the time.
func newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history NAME",
		Short: "Show a branch's audit trail (who transitioned it, when, and why)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var rows []api.Transition
			if c := serverClient(cmd); c != nil {
				r, err := c.BranchHistory(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				rows = r
			} else {
				reg, err := openRegistry()
				if err != nil {
					return err
				}
				defer reg.Close()
				ts, err := reg.BranchHistory(args[0])
				if err != nil {
					return err
				}
				for _, t := range ts {
					rows = append(rows, api.Transition{
						FromState: t.FromState, ToState: t.ToState,
						Reason: t.Reason, Actor: t.Actor, At: t.At,
					})
				}
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "AT\tFROM\tTO\tACTOR\tREASON")
			for _, t := range rows {
				from := t.FromState
				if from == "" {
					from = "-"
				}
				reason := t.Reason
				if reason == "" {
					reason = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.At, from, t.ToState, t.Actor, reason)
			}
			return w.Flush()
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
				// rotate mode: the server returns a per-branch password —
				// include it so the DSNs are copy-pasteable
				auth := userInfo(b.User, b.Password)
				fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@%s:%d/%s\n", auth, directHost, b.Port, b.Database)
				fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@%s:6432/%s\n", auth, serverHost, b.ProxyDatabase)
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
			fmt.Fprintf(cmd.OutOrStdout(), "postgres://%s@%s:%d/%s\n", userInfo(s.ConnUser, b.Password), b.Host, b.Port, s.ConnDB)
			return nil
		},
	}
}

// userInfo renders the DSN userinfo part: user, or user:password when the
// branch carries its own rotated password.
func userInfo(user, password string) string {
	if password == "" {
		return user
	}
	return user + ":" + url.QueryEscape(password)
}
