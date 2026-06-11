// Package cli wires cobra commands to the engine (local mode) or to a
// running branchd via the REST API (server mode: --server / PGBRANCH_SERVER).
// Engine construction is lazy (inside RunE) so --help and tests never touch
// Docker.
package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgbranch/internal/apiclient"
	"github.com/abd-ulbasit/pgbranch/internal/config"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pgb",
		Short:         "pgbranch — git branch for Postgres",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().String("server", os.Getenv("PGBRANCH_SERVER"),
		"branchd base URL (http:// or https://, e.g. http://localhost:7070); enables server mode [env PGBRANCH_SERVER, token from PGBRANCH_TOKEN; PGBRANCH_TLS_SKIP_VERIFY=1 for self-signed certs]")
	root.AddCommand(newSourceCmd(), newBranchCmd(), newConnectCmd(), newDiffCmd())
	return root
}

// serverClient returns a REST client when server mode is enabled, else nil
// (commands then embed the engine locally). Don't run local mode while a
// branchd is using the same registry — SQLite is single-writer.
func serverClient(cmd *cobra.Command) *apiclient.Client {
	s, _ := cmd.Root().PersistentFlags().GetString("server")
	if s == "" {
		return nil
	}
	return apiclient.New(s, os.Getenv("PGBRANCH_TOKEN"))
}

// openRegistry opens just the registry (no runtime driver) for commands that
// only read/write metadata; callers must Close it.
func openRegistry() (*registry.Registry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := cfg.EnsureHome(); err != nil {
		return nil, err
	}
	return registry.Open(cfg.RegistryPath)
}

// open builds the engine; callers must Close the returned registry.
func open() (*engine.Engine, *registry.Registry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.EnsureHome(); err != nil {
		return nil, nil, err
	}
	reg, err := registry.Open(cfg.RegistryPath)
	if err != nil {
		return nil, nil, err
	}
	drv, err := runtime.NewDockerDriver()
	if err != nil {
		reg.Close()
		return nil, nil, err
	}
	return engine.New(reg, drv, cfg.PostgresImage), reg, nil
}
