package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage API tokens (admin-only on a running branchd)"}
	cmd.AddCommand(newTokenCreateCmd(), newTokenLsCmd(), newTokenRevokeCmd())
	return cmd
}

func newTokenCreateCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Mint an API token (the token is printed once and never recoverable)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !registry.ValidRole(role) {
				return fmt.Errorf("invalid --role %q: want admin, operator or viewer", role)
			}
			var token string
			if c := serverClient(cmd); c != nil {
				t, err := c.CreateToken(cmd.Context(), args[0], role)
				if err != nil {
					return err
				}
				token = t
			} else {
				reg, err := openRegistry()
				if err != nil {
					return err
				}
				defer reg.Close()
				t, err := reg.CreateAPIToken(args[0], role)
				if err != nil {
					return err
				}
				token = t
			}
			fmt.Fprintln(cmd.OutOrStdout(), token)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", registry.RoleViewer, "token role: admin, operator or viewer")
	return cmd
}

func newTokenLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List API tokens (names and roles only — token values are never shown)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tROLE\tCREATED")
			if c := serverClient(cmd); c != nil {
				tokens, err := c.ListTokens(cmd.Context())
				if err != nil {
					return err
				}
				for _, t := range tokens {
					fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.Role, t.CreatedAt)
				}
				return w.Flush()
			}
			reg, err := openRegistry()
			if err != nil {
				return err
			}
			defer reg.Close()
			tokens, err := reg.ListAPITokens()
			if err != nil {
				return err
			}
			for _, t := range tokens {
				fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.Role, t.CreatedAt)
			}
			return w.Flush()
		},
	}
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke NAME",
		Short: "Revoke an API token by name",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := serverClient(cmd); c != nil {
				if err := c.RevokeToken(cmd.Context(), args[0]); err != nil {
					return err
				}
			} else {
				reg, err := openRegistry()
				if err != nil {
					return err
				}
				defer reg.Close()
				if err := reg.RevokeAPIToken(args[0]); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "token %q revoked\n", args[0])
			return nil
		},
	}
}
