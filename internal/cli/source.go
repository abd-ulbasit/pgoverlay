package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

func newSourceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Manage branch sources (seeded data dirs)"}
	cmd.AddCommand(newSourceAddCmd(), newSourceLsCmd(), newSourceRmCmd(), newSourceRefreshCmd(),
		newSourceSetMaskCmd(), newSourceGetMaskCmd(), newSourceClearMaskCmd())
	return cmd
}

// sourcePassword returns the password for the seed connection: none with
// --no-password (trust, peer or certificate authentication), else the value
// of the environment variable named by --password-env, which must be set.
func sourcePassword(env string, noPassword bool) (string, error) {
	if noPassword {
		return "", nil
	}
	if strings.TrimSpace(env) == "" {
		return "", fmt.Errorf("--password-env must name an environment variable (or pass --no-password)")
	}
	if password := os.Getenv(env); password != "" {
		return password, nil
	}
	return "", fmt.Errorf("the source password is read from $%s, which is empty or unset: set it, "+
		"point --password-env at the variable that holds the password, "+
		"or pass --no-password for a source that needs none (trust, peer or certificate authentication)", env)
}

// passwordFlags registers --password-env and --no-password on cmd.
func passwordFlags(cmd *cobra.Command, env *string, none *bool) {
	cmd.Flags().StringVar(env, "password-env", "PGPASSWORD", "env var holding the source password")
	cmd.Flags().BoolVar(none, "no-password", false, "connect to the source without a password (trust, peer or certificate authentication)")
	cmd.MarkFlagsMutuallyExclusive("password-env", "no-password")
}

func newSourceAddCmd() *cobra.Command {
	var host, user, db, network, pgVersion, passwordEnv, via, image string
	var noPassword bool
	var dumpSchemas []string
	var port int
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Register a source and seed it from a running Postgres",
		Long: `Register a source and seed it from a running Postgres.

Seeding methods (--via):
  basebackup  pg_basebackup, a physical copy (default). Needs a user with
              REPLICATION privilege and wal_level=replica on the source.
  dump        pg_dump piped into a freshly initialized cluster, a logical
              copy. Needs only a normal user (no REPLICATION) and works with
              managed Postgres (Supabase, Neon, RDS, Cloud SQL). Optionally
              scoped with repeatable --dump-schema. --pg-version must be >=
              the remote server's major version (pg_dump cannot dump newer
              servers); branches run on --pg-version.`,
		Args: nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			// MarkFlagRequired only checks that --host was passed; `--host ""`
			// (e.g. from an empty $(docker inspect ...)) would otherwise make
			// pg_basebackup fall back to a local Unix socket.
			if err := nonEmptyFlag("host", host,
				"pass the source Postgres host as reachable from branch containers (host.docker.internal for a database on this machine)"); err != nil {
				return err
			}
			for _, f := range []struct{ name, value string }{{"user", user}, {"database", db}, {"pg-version", pgVersion}} {
				if err := nonEmptyFlag(f.name, f.value, ""); err != nil {
					return err
				}
			}
			if err := validPort("port", port); err != nil {
				return err
			}
			// Validated here as well as by the API: in local mode nothing
			// else checks it, and any value but "dump" used to fall through
			// to pg_basebackup.
			if via != registry.SeedViaBasebackup && via != registry.SeedViaDump {
				return fmt.Errorf("invalid --via %q: want %q or %q", via, registry.SeedViaBasebackup, registry.SeedViaDump)
			}
			if len(dumpSchemas) > 0 && via != registry.SeedViaDump {
				return fmt.Errorf("--dump-schema is only valid with --via dump")
			}
			password, err := sourcePassword(passwordEnv, noPassword)
			if err != nil {
				return err
			}
			if c := serverClient(cmd); c != nil {
				s, err := c.CreateSource(cmd.Context(), api.CreateSourceRequest{
					Name: args[0], Host: host, Port: port, User: user,
					Database: db, Network: network, PGVersion: pgVersion, Password: password,
					Via: via, DumpSchemas: dumpSchemas, Image: image,
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
				ConnHost: host, ConnPort: port, ConnUser: user, ConnDB: db, Network: network,
				SeedVia: via, DumpSchemas: dumpSchemas, Image: image}
			if err := e.AddSource(cmd.Context(), s, password); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source %q seeded and ready\n", s.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "source Postgres host (as reachable from containers; use host.docker.internal for a host-local DB)")
	cmd.Flags().IntVar(&port, "port", 5432, "source Postgres port")
	cmd.Flags().StringVar(&user, "user", "postgres", "seed user (REPLICATION privilege for --via basebackup; a normal user suffices for --via dump)")
	cmd.Flags().StringVar(&db, "database", "postgres", "database name recorded for connection strings (and dumped with --via dump)")
	cmd.Flags().StringVar(&network, "network", "", "docker network from which the source is reachable")
	cmd.Flags().StringVar(&pgVersion, "pg-version", "17", "source Postgres major version, 14-18 (branch image must match; with --via dump it must be >= the remote major)")
	passwordFlags(cmd, &passwordEnv, &noPassword)
	cmd.Flags().StringVar(&via, "via", registry.SeedViaBasebackup, `seeding method: "basebackup" or "dump" (managed Postgres: Supabase/Neon/RDS)`)
	cmd.Flags().StringArrayVar(&dumpSchemas, "dump-schema", nil, "schema to dump (repeatable; --via dump only; default: the whole database)")
	cmd.Flags().StringVar(&image, "image", "", "container image for the seed helpers and every branch (default postgres:<pg-version>); must carry the source's extensions, locales and libc, e.g. postgis/postgis:17-3.5")
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
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
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

func newSourceSetMaskCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-mask NAME FILE...",
		Short: "Replace a source's masking SQL (applied, in argument order, inside every new/reset branch)",
		Long: "Replace a source's masking SQL with the given files, applied in argument order inside every " +
			"new or reset branch. Use `pgb source clear-mask NAME` to remove masking.",
		Args: nonEmptyArgs(cobra.MinimumNArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			scripts := make([]api.MaskScript, 0, len(args)-1)
			for _, file := range args[1:] {
				sql, err := os.ReadFile(file)
				if err != nil {
					return err
				}
				scripts = append(scripts, api.MaskScript{Name: filepath.Base(file), SQL: string(sql)})
			}
			if err := storeMaskScripts(cmd, name, scripts); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source %q masking set (%d script(s))\n", name, len(scripts))
			return nil
		},
	}
}

// newSourceClearMaskCmd removes a source's masking. It is a command of its
// own, not `set-mask NAME` with no files, so a forgotten file argument can
// never silently unmask new branches.
func newSourceClearMaskCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "clear-mask NAME",
		Short: "Remove a source's masking SQL (new and reset branches are no longer masked)",
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := storeMaskScripts(cmd, name, []api.MaskScript{}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "source %q masking cleared; new and reset branches are no longer masked\n", name)
			return nil
		},
	}
}

// storeMaskScripts replaces the source's masking scripts (empty clears them)
// through the API in server mode, else directly in the registry.
func storeMaskScripts(cmd *cobra.Command, name string, scripts []api.MaskScript) error {
	if c := serverClient(cmd); c != nil {
		_, err := c.SetMaskScripts(cmd.Context(), name, scripts)
		return err
	}
	reg, err := openRegistry()
	if err != nil {
		return err
	}
	defer reg.Close()
	src, err := reg.GetSourceByName(name)
	if err != nil {
		return fmt.Errorf("source %q: %w", name, err)
	}
	rs := make([]registry.MaskScript, len(scripts))
	for i, sc := range scripts {
		rs[i] = registry.MaskScript{Name: sc.Name, SQL: sc.SQL}
	}
	return reg.SetMaskScripts(src.ID, rs)
}

func newSourceGetMaskCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-mask NAME",
		Short: "List a source's masking scripts in application order",
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var names []string
			if c := serverClient(cmd); c != nil {
				scripts, err := c.GetMaskScripts(cmd.Context(), name)
				if err != nil {
					return err
				}
				for _, sc := range scripts {
					names = append(names, sc.Name)
				}
			} else {
				reg, err := openRegistry()
				if err != nil {
					return err
				}
				defer reg.Close()
				src, err := reg.GetSourceByName(name)
				if err != nil {
					return fmt.Errorf("source %q: %w", name, err)
				}
				scripts, err := reg.GetMaskScripts(src.ID)
				if err != nil {
					return err
				}
				for _, sc := range scripts {
					names = append(names, sc.Name)
				}
			}
			for _, n := range names {
				fmt.Fprintln(cmd.OutOrStdout(), n)
			}
			return nil
		},
	}
}

func newSourceRefreshCmd() *cobra.Command {
	var passwordEnv string
	var noPassword bool
	cmd := &cobra.Command{
		Use:   "refresh NAME",
		Short: "Re-seed a source into a new generation (existing branches keep their snapshot)",
		Args:  nonEmptyArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			password, err := sourcePassword(passwordEnv, noPassword)
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
	passwordFlags(cmd, &passwordEnv, &noPassword)
	return cmd
}
