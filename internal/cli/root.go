// Package cli wires cobra commands to the engine (local mode) or to a
// running branchd via the REST API (server mode: --server / PGOVERLAY_SERVER).
// Engine construction is lazy (inside RunE) so --help and tests never touch
// Docker.
package cli

import (
	"fmt"
	"os"
	"os/user"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
	"github.com/abd-ulbasit/pgoverlay/internal/config"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pgb",
		Short:         "pgoverlay — git branch for Postgres",
		SilenceUsage:  true,
		SilenceErrors: false,
		// Local-mode commands call the engine/registry directly, so stamp the
		// OS user as the audit actor; otherwise their transitions would be
		// journaled as the daemon (system:reconcile). Server mode ignores it:
		// branchd records the actor behind the bearer token.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cmd.SetContext(registry.WithActor(cmd.Context(), localActor()))
		},
	}
	root.PersistentFlags().String("server", os.Getenv("PGOVERLAY_SERVER"),
		"branchd base URL (http:// or https://, e.g. http://localhost:7070); enables server mode [env PGOVERLAY_SERVER, token from PGOVERLAY_TOKEN; PGOVERLAY_TLS_SKIP_VERIFY=1 for self-signed certs]")
	root.AddCommand(newSourceCmd(), newBranchCmd(), newConnectCmd(), newDiffCmd(), newHistoryCmd(), newDoctorCmd(), newGCCmd(), newTokenCmd())
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
	return apiclient.New(s, os.Getenv("PGOVERLAY_TOKEN"))
}

// openRegistry opens just the registry (no runtime driver) for commands that
// only read/write metadata; callers must Close it.
func openRegistry() (*registry.Registry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return openRegistryAt(cfg)
}

// openRegistryAt opens the registry under cfg's state dir with the same
// at-rest keys branchd uses, so local mode can read passwords a branchd on this
// state dir stored: the dedicated key ($PGOVERLAY_SECRET_KEY, else
// $PGOVERLAY_SECRET_KEY_FILE, else <state dir>/secret.key; never generated
// here), the retired keys branchd accepts, and sha256($PGOVERLAY_TOKEN) for
// legacy rows. A key that cannot be
// loaded is a warning, not an error: only rotated passwords need it, and they
// then read as unavailable.
func openRegistryAt(cfg *config.Config) (*registry.Registry, error) {
	if err := cfg.EnsureHome(); err != nil {
		return nil, err
	}
	reg, err := registry.Open(cfg.RegistryPath)
	if err != nil {
		return nil, err
	}
	key, _, err := cfg.LoadSecretKey(os.Getenv(config.SecretKeyFileEnv), false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (rotated branch passwords will read as unavailable)\n", err)
		key = nil
	}
	previous, err := cfg.PreviousSecretKeys(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		previous = nil
	}
	if err := reg.SetSecretKeys(registry.SecretKeys{
		Primary:  key,
		Previous: previous,
		Legacy:   [][]byte{registry.LegacyTokenKey(os.Getenv("PGOVERLAY_TOKEN"))},
	}); err != nil {
		reg.Close()
		return nil, err
	}
	return reg, nil
}

// localActor is the audit identity recorded for mutations made in local mode
// (direct registry access, no API token): "local:<os user>".
func localActor() registry.Actor {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return registry.LocalActor(name)
}

// open builds the engine; callers must Close the returned registry.
func open() (*engine.Engine, *registry.Registry, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	reg, err := openRegistryAt(cfg)
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
