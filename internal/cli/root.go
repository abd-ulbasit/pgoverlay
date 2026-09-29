// Package cli wires cobra commands to the engine (local mode) or to a
// running branchd via the REST API (server mode: --server / PGOVERLAY_SERVER).
// Engine construction is lazy (inside RunE) so --help and tests never touch
// Docker.
package cli

import (
	"fmt"
	"os"
	"os/user"
	"strings"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
	"github.com/abd-ulbasit/pgoverlay/internal/config"
	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
	"github.com/abd-ulbasit/pgoverlay/internal/runtime"
	"github.com/abd-ulbasit/pgoverlay/internal/version"
)

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pgb",
		Short:         "pgoverlay — git branch for Postgres",
		SilenceUsage:  true,
		SilenceErrors: false,
		// --version prints the same line as `pgb version`.
		Version: version.String(),
		// Runs once for every subcommand (cobra runs only the nearest
		// persistent hook, so this is the one place for both steps):
		//   - Local-mode commands call the engine/registry directly, so the
		//     OS user is stamped as the audit actor; otherwise their
		//     transitions would be journaled as the daemon (system:reconcile).
		//     Server mode ignores it: branchd records the actor behind the
		//     bearer token.
		//   - A malformed --server fails here with a usable message, before
		//     any request is built, instead of a Go transport error
		//     ("unsupported protocol scheme").
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cmd.SetContext(registry.WithActor(cmd.Context(), localActor()))
			s, _ := cmd.Root().PersistentFlags().GetString("server")
			if s == "" {
				return nil
			}
			if err := apiclient.ValidateBaseURL(s); err != nil {
				return fmt.Errorf("invalid --server (or PGOVERLAY_SERVER): %w", err)
			}
			return nil
		},
	}
	root.SetVersionTemplate("pgb {{.Version}}\n")
	root.PersistentFlags().String("server", os.Getenv("PGOVERLAY_SERVER"),
		"branchd base URL (http:// or https://, e.g. http://localhost:7070); enables server mode "+
			"[env PGOVERLAY_SERVER; token from PGOVERLAY_TOKEN; PGOVERLAY_CA_CERT=<pem file> trusts a private or self-signed CA; "+
			"PGOVERLAY_TLS_SKIP_VERIFY=1 disables certificate verification (insecure, last resort)]")
	root.AddCommand(newSourceCmd(), newBranchCmd(), newConnectCmd(), newDiffCmd(), newHistoryCmd(),
		newDoctorCmd(), newGCCmd(), newTokenCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the pgb version",
		Args:  cobra.NoArgs,
		// No --server check: printing the version must work whatever the
		// environment holds.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Fprintln(cmd.OutOrStdout(), "pgb "+version.String())
		},
	}
}

// serverClient returns a REST client when server mode is enabled, else nil
// (commands then embed the engine locally). Don't run local mode while a
// branchd is using the same registry — SQLite is single-writer. The root
// command has already validated the URL.
func serverClient(cmd *cobra.Command) *apiclient.Client {
	s, _ := cmd.Root().PersistentFlags().GetString("server")
	if s == "" {
		return nil
	}
	token := os.Getenv("PGOVERLAY_TOKEN")
	if token == "" {
		fmt.Fprintln(cmd.ErrOrStderr(), "pgb: PGOVERLAY_TOKEN is not set; branchd rejects requests without a bearer token")
	}
	return apiclient.New(s, token)
}

// nonEmptyArgs wraps a cobra positional-args validator and also rejects empty or
// blank arguments: cobra counts `pgb connect ""` as one argument, which would
// otherwise reach the engine or the API as an empty name. The error names the
// placeholder from the command's Use line ("NAME must not be empty").
func nonEmptyArgs(base cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, a []string) error {
		if err := base(cmd, a); err != nil {
			return err
		}
		placeholders := strings.Fields(cmd.Use)[1:]
		for i, v := range a {
			if strings.TrimSpace(v) != "" {
				continue
			}
			name := fmt.Sprintf("argument %d", i+1)
			if n := len(placeholders); n > 0 {
				name = strings.TrimSuffix(placeholders[min(i, n-1)], "...")
			}
			return fmt.Errorf("%s: %s must not be empty", cmd.CommandPath(), name)
		}
		return nil
	}
}

// nonEmptyFlag rejects a flag passed with an empty or blank value: cobra's
// MarkFlagRequired only checks that the flag was given, so `--host ""` would
// otherwise pass. hint, when set, tells the user what to pass instead.
func nonEmptyFlag(name, value, hint string) error {
	if strings.TrimSpace(value) != "" {
		return nil
	}
	if hint != "" {
		return fmt.Errorf("--%s must not be empty: %s", name, hint)
	}
	return fmt.Errorf("--%s must not be empty", name)
}

// validPort rejects TCP ports outside 1-65535.
func validPort(name string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("--%s %d is not a valid TCP port (1-65535)", name, port)
	}
	return nil
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
	opts, err := cowOptions(cfg)
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
	return engine.New(reg, drv, cfg.PostgresImage, opts...), reg, nil
}
