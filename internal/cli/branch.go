package cli

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

func newBranchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "branch", Short: "Manage branches"}
	cmd.AddCommand(newBranchCreateCmd(), newBranchLsCmd(), newBranchDestroyCmd(), newBranchResetCmd(), newBranchRecoverCmd())
	return cmd
}

func newBranchCreateCmd() *cobra.Command {
	var from, fromBranch string
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an instant copy-on-write branch (off a source, or off another branch)",
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (from == "") == (fromBranch == "") {
				return fmt.Errorf("exactly one of --from (source) or --from-branch (parent branch) is required")
			}
			if ttl < 0 {
				return fmt.Errorf("--ttl %s is negative; use 0 for a branch that never expires", ttl)
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
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "expire the branch after this duration (e.g. 24h; 0 = never). "+
		"Expired branches are destroyed by branchd's reconcile loop or by `pgb gc`; local mode does not reap them on its own")
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
			expired := 0
			now := time.Now()
			for _, b := range branches {
				row(b.Name, b.ParentBranchName, string(b.State), b.Port, b.ExpiresAt, b.CreatedAt,
					func() (int64, error) { return e.BranchUsage(cmd.Context(), b.Name) })
				if t, err := time.Parse(time.RFC3339, b.ExpiresAt); err == nil && t.Before(now) {
					expired++
				}
			}
			if err := w.Flush(); err != nil {
				return err
			}
			// Without branchd nothing reaps expired branches; say so rather
			// than let an EXPIRES in the past look like a bug.
			if expired > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: %d branch(es) are past their TTL; local mode does not reap them, run `pgb gc` to destroy them\n", expired)
			}
			return nil
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
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
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
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
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

func newBranchRecoverCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "recover NAME",
		Short: "Restart a failed branch on its existing data (keeps its writes; reset discards them)",
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			port := 0
			if c := serverClient(cmd); c != nil {
				b, err := c.RecoverBranch(cmd.Context(), args[0])
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
				b, err := e.RecoverBranch(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				port = b.Port
			}
			fmt.Fprintf(cmd.OutOrStdout(), "branch %q recovered (port %d)\n", args[0], port)
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
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
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

// defaultProxyPort is branchd's default --pg-addr port, assumed for servers
// that do not advertise their router address.
const defaultProxyPort = 6432

func newConnectCmd() *cobra.Command {
	var proxyHost string
	var proxyPort int
	cmd := &cobra.Command{
		Use:   "connect NAME",
		Short: "Print connection strings for a branch",
		Long: `Print connection strings for a ready branch.

In server mode it prints two URLs: the branch's own Postgres (direct) and
branchd's wire-protocol router (database "db@branch"). The router address is
taken from --proxy-host/--proxy-port when given, else from what branchd
advertises (--advertise-proxy-addr), else the --server host and port 6432.
A branch that is not ready (creating, resetting, failed) is refused.`,
		Args: nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if proxyPort != 0 {
				if err := validPort("proxy-port", proxyPort); err != nil {
					return err
				}
			}
			name := args[0]
			if c := serverClient(cmd); c != nil {
				b, err := c.GetBranch(cmd.Context(), name)
				if err != nil {
					return err
				}
				if err := requireReady(name, b.State); err != nil {
					return err
				}
				if b.PasswordUnavailable {
					return errPasswordUnavailable(b.Name, false)
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
				// the router: explicit flags, else what branchd advertises,
				// else the API host on the default port (older servers)
				pxHost := firstNonEmpty(proxyHost, b.ProxyHost, serverHost)
				pxPort := proxyPort
				if pxPort == 0 {
					pxPort = b.ProxyPort
				}
				if pxPort == 0 {
					pxPort = defaultProxyPort
				}
				// rotate mode: the server returns a per-branch password —
				// include it so the DSNs are copy-pasteable
				fmt.Fprintln(cmd.OutOrStdout(), postgresURL(b.User, b.Password, directHost, b.Port, b.Database))
				fmt.Fprintln(cmd.OutOrStdout(), postgresURL(b.User, b.Password, pxHost, pxPort, b.ProxyDatabase))
				return nil
			}
			// metadata only: no container runtime needed
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			b, err := reg.GetBranchByName(name)
			if err != nil {
				return err
			}
			if err := requireReady(name, string(b.State)); err != nil {
				return err
			}
			if b.PasswordUnavailable {
				return errPasswordUnavailable(b.Name, true)
			}
			s, err := reg.GetSourceByID(b.SourceID)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), postgresURL(s.ConnUser, b.Password, b.Host, b.Port, s.ConnDB))
			return nil
		},
	}
	cmd.Flags().StringVar(&proxyHost, "proxy-host", "", "host of branchd's Postgres router for the proxy URL (default: what branchd advertises, else the --server host)")
	cmd.Flags().IntVar(&proxyPort, "proxy-port", 0, "port of branchd's Postgres router for the proxy URL (default: what branchd advertises, else 6432)")
	return cmd
}

// errPasswordUnavailable explains a branch whose rotated password cannot be
// decrypted: printing a DSN without it would silently fall back to the
// source's credentials, which the branch no longer accepts.
func errPasswordUnavailable(name string, local bool) error {
	where := "branchd's at-rest key changed since it was stored"
	if local {
		where = "local mode reads the key from $PGOVERLAY_SECRET_KEY, $PGOVERLAY_SECRET_KEY_FILE or <state dir>/secret.key"
	}
	return fmt.Errorf("branch %q has a rotated password that cannot be decrypted with the configured at-rest key (%s); "+
		"reset it (pgb branch reset %s) to mint a new one", name, where, name)
}

// requireReady refuses to print a DSN for a branch that is not ready: before
// its container runs a row has no host/port (a failed create prints port 0),
// and a creating or resetting branch does not accept connections yet.
func requireReady(name, state string) error {
	if state == string(registry.BranchReady) {
		return nil
	}
	hint := ""
	switch registry.BranchState(state) {
	case registry.BranchCreating, registry.BranchResetting:
		hint = "; wait until `pgb branch ls` shows it ready"
	case registry.BranchFailed:
		hint = fmt.Sprintf("; see `pgb history %s` for the cause, then `pgb branch recover %s` (keeps its data), reset or destroy it", name, name)
	}
	return fmt.Errorf("branch %q is %s, not ready%s", name, state, hint)
}

// postgresURL renders a libpq connection URI. User, password and database are
// percent-encoded (RFC 3986: a space is %20, never '+'), so an Azure-style
// "app@server" login or a database named "my db" survive; an IPv6 host is
// bracketed. The router's "db@branch" database keeps its '@', which is legal
// in a URI path.
func postgresURL(user, password, host string, port int, db string) string {
	ui := url.User(user)
	if password != "" {
		ui = url.UserPassword(user, password)
	}
	return "postgres://" + ui.String() + "@" + net.JoinHostPort(host, strconv.Itoa(port)) + "/" + url.PathEscape(db)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
