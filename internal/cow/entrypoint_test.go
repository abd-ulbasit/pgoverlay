package cow

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// runEntrypoint executes a branch entrypoint with sh against pgdata. mount,
// mkdir and chown are stubbed (they need root or touch /pgoverlay), and the
// stub docker-entrypoint.sh records the postgres argv instead of starting it.
func runEntrypoint(t *testing.T, script, pgdata string) []string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	stubs := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	for name, body := range map[string]string{
		"mount":                "exit 0",
		"mkdir":                "exit 0",
		"chown":                "exit 0",
		"docker-entrypoint.sh": `for a in "$@"; do printf '%s\n' "$a"; done > "$ARGS_FILE"`,
	} {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	scriptPath := filepath.Join(t.TempDir(), "entrypoint.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, scriptPath)
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PGDATA="+pgdata, "PGOVERLAY_LOWERS=/pgoverlay/lower/0", "ARGS_FILE="+argsFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, out)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// settings returns the -c name=value pairs of a postgres argv.
func settings(t *testing.T, argv []string) map[string]string {
	t.Helper()
	if len(argv) == 0 || argv[0] != "postgres" {
		t.Fatalf("argv = %q, want postgres first", argv)
	}
	out := map[string]string{}
	for i := 1; i < len(argv); i += 2 {
		if argv[i] != "-c" || i+1 >= len(argv) {
			t.Fatalf("argv = %q: want only -c name=value pairs after postgres", argv)
		}
		k, v, ok := strings.Cut(argv[i+1], "=")
		if !ok {
			t.Fatalf("setting %q has no =", argv[i+1])
		}
		out[k] = v
	}
	return out
}

var entrypoints = map[string]string{"overlay": EntrypointScript, "direct": EntrypointScriptDirect}

// A branch of a standby source, seeded from a distro-packaged cluster
// (config outside the data dir, ssl on with system certificates): it must
// boot as a writable primary on the container's port and socket, with no
// replication, archiving or synchronous-standby setting reaching back to
// the source.
func TestEntrypointsNeutraliseInheritedServerConfig(t *testing.T) {
	for name, script := range entrypoints {
		t.Run(name, func(t *testing.T) {
			pgdata := t.TempDir()
			for _, f := range []string{"PG_VERSION", "standby.signal", "recovery.signal", "postmaster.pid"} {
				if err := os.WriteFile(filepath.Join(pgdata, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := settings(t, runEntrypoint(t, script, pgdata))

			for _, f := range []string{"standby.signal", "recovery.signal", "postmaster.pid"} {
				if _, err := os.Stat(filepath.Join(pgdata, f)); !os.IsNotExist(err) {
					t.Errorf("%s survived the entrypoint (stat err %v)", f, err)
				}
			}
			for _, f := range []string{"postgresql.conf", "pg_ident.conf"} {
				if b, err := os.ReadFile(filepath.Join(pgdata, f)); err != nil || len(b) != 0 {
					t.Errorf("%s: want a synthesised empty file, got %q (err %v)", f, b, err)
				}
			}
			hba, err := os.ReadFile(filepath.Join(pgdata, "pg_hba.conf"))
			if err != nil || string(hba) != "local all all trust\nhost all all all md5\n" {
				t.Errorf("pg_hba.conf = %q (err %v), want local trust + password auth over TCP", hba, err)
			}

			want := map[string]string{
				"recovery_init_sync_method": "syncfs",
				"port":                      "5432",
				"listen_addresses":          "*",
				"unix_socket_directories":   "/var/run/postgresql",
				"hba_file":                  pgdata + "/pg_hba.conf",
				"ident_file":                pgdata + "/pg_ident.conf",
				"logging_collector":         "off",
				"primary_conninfo":          "",
				"primary_slot_name":         "",
				"restore_command":           "",
				"archive_cleanup_command":   "",
				"recovery_end_command":      "",
				"archive_mode":              "off",
				"synchronous_standby_names": "",
				"ssl":                       "off",
			}
			for k, v := range want {
				if g, ok := got[k]; !ok || g != v {
					t.Errorf("-c %s = %q (set %v), want %q", k, g, ok, v)
				}
			}
			if len(got) != len(want) {
				t.Errorf("settings = %v, want exactly %v", got, want)
			}
		})
	}
}

// A source whose config and certificates live in the data dir keeps them:
// nothing is overwritten and TLS stays as configured.
func TestEntrypointsKeepConfigInsideTheDataDir(t *testing.T) {
	files := map[string]string{
		"PG_VERSION":      "17\n",
		"postgresql.conf": "ssl = on\nshared_buffers = 256MB\n",
		"pg_hba.conf":     "hostssl all all all scram-sha-256\n",
		"pg_ident.conf":   "# MAPNAME SYSTEM-USERNAME PG-USERNAME\n",
		"server.crt":      "cert",
	}
	for name, script := range entrypoints {
		t.Run(name, func(t *testing.T) {
			pgdata := t.TempDir()
			for f, content := range files {
				if err := os.WriteFile(filepath.Join(pgdata, f), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			argv := runEntrypoint(t, script, pgdata)
			for f, content := range files {
				if b, _ := os.ReadFile(filepath.Join(pgdata, f)); string(b) != content {
					t.Errorf("%s changed to %q", f, b)
				}
			}
			if slices.Contains(argv, "ssl=off") {
				t.Errorf("ssl forced off although server.crt is in the data dir: %q", argv)
			}
		})
	}
}
