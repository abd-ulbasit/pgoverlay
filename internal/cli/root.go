// Package cli wires cobra commands to the engine. Engine construction is
// lazy (inside RunE) so --help and tests never touch Docker.
package cli

import (
	"github.com/spf13/cobra"

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
	root.AddCommand(newSourceCmd(), newBranchCmd(), newConnectCmd())
	return root
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
