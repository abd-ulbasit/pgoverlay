package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/config"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

// localHome points local mode at a fresh state dir and returns it.
func localHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "state")
	t.Setenv("PGOVERLAY_HOME", home)
	t.Setenv("PGOVERLAY_SERVER", "")
	t.Setenv("PGOVERLAY_TOKEN", "")
	t.Setenv(config.SecretKeyEnv, "")
	t.Setenv(config.SecretKeyFileEnv, "")
	return home
}

// seedRotatedBranch writes what a rotating branchd leaves in the registry: a
// ready branch whose password is encrypted under key.
func seedRotatedBranch(t *testing.T, home string, key []byte, password string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(filepath.Join(home, "pgoverlay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if err := reg.SetSecretKey(key); err != nil {
		t.Fatal(err)
	}
	src := &registry.Source{Name: "main", PGVersion: "17", Volume: "v", ConnUser: "postgres", ConnDB: "app"}
	if err := reg.CreateSource(src); err != nil {
		t.Fatal(err)
	}
	b := &registry.Branch{Name: "pr-1", SourceID: src.ID, RWVolume: "rw"}
	if err := reg.CreateBranch(b); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetBranchPassword(b.ID, password); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkBranchReady(b.ID, "cid", "127.0.0.1", 54321); err != nil {
		t.Fatal(err)
	}
}

func writeKeyFile(t *testing.T, path string) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return key
}

// SECRETS-09/12: local mode reads the at-rest key branchd generated in the
// state dir, so it can use registries a rotating branchd wrote.
func TestLocalModeReadsStateDirKey(t *testing.T) {
	home := localHome(t)
	key := writeKeyFile(t, filepath.Join(home, config.SecretKeyFileName))
	seedRotatedBranch(t, home, key, "0123456789abcdef0123456789abcdef")

	out := run(t, "connect", "pr-1")
	if !strings.Contains(out, "postgres://postgres:0123456789abcdef0123456789abcdef@127.0.0.1:54321/app") {
		t.Fatalf("connect output %q lacks the decrypted password", out)
	}
	// history (another local registry command) works too
	if out := run(t, "history", "pr-1"); !strings.Contains(out, "ready") {
		t.Fatalf("history output %q", out)
	}
}

// Without the right key, local connect fails with an actionable message
// instead of printing a DSN that silently falls back to the source password.
func TestLocalModeConnectPasswordUnavailable(t *testing.T) {
	home := localHome(t)
	seedRotatedBranch(t, home, writeKeyFile(t, filepath.Join(t.TempDir(), "other.key")), "0123456789abcdef0123456789abcdef")

	_, err := runErr(t, "connect", "pr-1")
	if err == nil || !strings.Contains(err.Error(), "cannot be decrypted") || !strings.Contains(err.Error(), "pgb branch reset pr-1") {
		t.Fatalf("connect err=%v, want the password-unavailable explanation", err)
	}
}

func TestServerModeConnectPasswordUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.Branch{
			Name: "pr-9", State: "ready", Host: "10.0.0.7", Port: 32788,
			User: "postgres", PasswordUnavailable: true,
			Database: "postgres", ProxyDatabase: "postgres@pr-9",
		})
	}))
	defer ts.Close()
	t.Setenv("PGOVERLAY_TOKEN", "tok")

	out, err := runErr(t, "connect", "pr-9", "--server", ts.URL)
	if err == nil || !strings.Contains(err.Error(), "cannot be decrypted") {
		t.Fatalf("connect err=%v out=%q, want the password-unavailable explanation", err, out)
	}
	if strings.Contains(out, "postgres://") {
		t.Fatalf("printed a DSN without the password: %q", out)
	}
}

// Local-mode commands carry a local:<user> actor, so their transitions are
// not journaled as the daemon.
func TestLocalModeStampsLocalActor(t *testing.T) {
	localHome(t)
	t.Setenv("USER", "fallback-user")
	var got registry.Actor
	root := NewRootCmd()
	root.AddCommand(&cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, args []string) error {
		got = registry.ActorFromContext(cmd.Context())
		return nil
	}})
	root.SetArgs([]string{"probe"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.String(), registry.LocalActorPrefix) || got.String() == registry.LocalActorPrefix {
		t.Fatalf("local actor = %q, want local:<user>", got.String())
	}
}
