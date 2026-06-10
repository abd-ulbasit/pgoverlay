package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func newSourceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Manage branch sources (seeded data dirs)"}
	cmd.AddCommand(newSourceAddCmd(), newSourceLsCmd())
	return cmd
}

func newSourceAddCmd() *cobra.Command {
	var host, user, db, network, pgVersion, passwordEnv string
	var port int
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Register a source and seed it from a running Postgres (needs REPLICATION privilege)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			password := os.Getenv(passwordEnv)
			if password == "" {
				return fmt.Errorf("password env %q is empty", passwordEnv)
			}
			e, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			s := &registry.Source{Name: args[0], PGVersion: pgVersion,
				ConnHost: host, ConnPort: port, ConnUser: user, ConnDB: db, Network: network}
			if err := e.AddSource(cmd.Context(), s, password); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source %q seeded and ready\n", s.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "source Postgres host (as reachable from containers; use host.docker.internal for a host-local DB)")
	cmd.Flags().IntVar(&port, "port", 5432, "source Postgres port")
	cmd.Flags().StringVar(&user, "user", "postgres", "user with REPLICATION privilege")
	cmd.Flags().StringVar(&db, "database", "postgres", "database name recorded for connection strings")
	cmd.Flags().StringVar(&network, "network", "", "docker network from which the source is reachable")
	cmd.Flags().StringVar(&pgVersion, "pg-version", "17", "source Postgres major version (branch image must match)")
	cmd.Flags().StringVar(&passwordEnv, "password-env", "PGPASSWORD", "env var holding the source password")
	cmd.MarkFlagRequired("host")
	return cmd
}

func newSourceLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List sources",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			sources, err := reg.ListSources()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPG\tSTATE\tCREATED")
			for _, s := range sources {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Name, s.PGVersion, s.State, s.CreatedAt)
			}
			return w.Flush()
		},
	}
}
