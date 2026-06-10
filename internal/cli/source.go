package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func newSourceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Manage branch sources (seeded data dirs)"}
	cmd.AddCommand(newSourceAddCmd(), newSourceLsCmd(), newSourceRmCmd(), newSourceRefreshCmd())
	return cmd
}

func passwordFromEnv(env string) (string, error) {
	password := os.Getenv(env)
	if password == "" {
		return "", fmt.Errorf("password env %q is empty", env)
	}
	return password, nil
}

func newSourceAddCmd() *cobra.Command {
	var host, user, db, network, pgVersion, passwordEnv string
	var port int
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Register a source and seed it from a running Postgres (needs REPLICATION privilege)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			password, err := passwordFromEnv(passwordEnv)
			if err != nil {
				return err
			}
			if c := serverClient(cmd); c != nil {
				s, err := c.CreateSource(cmd.Context(), api.CreateSourceRequest{
					Name: args[0], Host: host, Port: port, User: user,
					Database: db, Network: network, PGVersion: pgVersion, Password: password,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "source %q seeded and ready\n", s.Name)
				return nil
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
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPG\tSTATE\tGEN\tCREATED")
			if c := serverClient(cmd); c != nil {
				sources, err := c.ListSources(cmd.Context())
				if err != nil {
					return err
				}
				for _, s := range sources {
					fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", s.Name, s.PGVersion, s.State, s.Generation, s.CreatedAt)
				}
				return w.Flush()
			}
			_, reg, err := open()
			if err != nil {
				return err
			}
			defer reg.Close()
			sources, err := reg.ListSources()
			if err != nil {
				return err
			}
			for _, s := range sources {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", s.Name, s.PGVersion, s.State, s.Generation, s.CreatedAt)
			}
			return w.Flush()
		},
	}
}

func newSourceRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME",
		Short: "Remove a source (refused while it has live branches)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if c := serverClient(cmd); c != nil {
				if err := c.RemoveSource(cmd.Context(), args[0]); err != nil {
					return err
				}
			} else {
				e, reg, err := open()
				if err != nil {
					return err
				}
				defer reg.Close()
				if err := e.RemoveSource(cmd.Context(), args[0]); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source %q removed\n", args[0])
			return nil
		},
	}
}

func newSourceRefreshCmd() *cobra.Command {
	var passwordEnv string
	cmd := &cobra.Command{
		Use:   "refresh NAME",
		Short: "Re-seed a source into a new generation (existing branches keep their snapshot)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			password, err := passwordFromEnv(passwordEnv)
			if err != nil {
				return err
			}
			gen := 0
			if c := serverClient(cmd); c != nil {
				s, err := c.RefreshSource(cmd.Context(), args[0], password)
				if err != nil {
					return err
				}
				gen = s.Generation
			} else {
				e, reg, err := open()
				if err != nil {
					return err
				}
				defer reg.Close()
				if err := e.RefreshSource(cmd.Context(), args[0], password); err != nil {
					return err
				}
				s, err := reg.GetSourceByName(args[0])
				if err != nil {
					return err
				}
				gen = s.Generation
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source %q refreshed (generation %d)\n", args[0], gen)
			return nil
		},
	}
	cmd.Flags().StringVar(&passwordEnv, "password-env", "PGPASSWORD", "env var holding the source password")
	return cmd
}
