package cow

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// entrypointRun is one sandboxed run of a branch entrypoint (see
// runEntrypointWith).
type entrypointRun struct {
	argv    []string // postgres argv handed to docker-entrypoint.sh
	preload string   // LD_PRELOAD docker-entrypoint.sh was started with
	cowMode []string // /pgoverlay/rw/cow-mode, by line ("" lines dropped)
	stderr  string
	root    string // the sandbox that stands in for /
	out     string // where the stubs record what they saw
}

// entrypointEnv configures the sandboxed tools of one run.
type entrypointEnv struct {
	// env is added to the entrypoint's environment (PGOVERLAY_LAZYRW, ...).
	env []string
	// arch is what uname -m prints (default x86_64).
	arch string
	// musl adds a musl loader to the sandbox's /lib.
	musl bool
	// selftest is how the self-test's overlay mount behaves: "pass" (default)
	// behaves like a kernel that re-targets read-only fds after a copy-up,
	// "fail" refuses the mount.
	selftest string
	// good lists the lazyrw variants that "load" into the stub postgres: a
	// preload of any other build is silently ignored, as musl does.
	good []string
	// builds lists the variants installed in the sandbox's lazyrw dir
	// (default: all of them).
	builds []string
	// noRunas leaves gosu out of the sandbox.
	noRunas bool
}

// runEntrypoint runs script with the default sandbox (see runEntrypointWith)
// and returns the postgres argv.
func runEntrypoint(t *testing.T, script, pgdata string) []string {
	t.Helper()
	return runEntrypointWith(t, script, pgdata, entrypointEnv{good: []string{LazyRWGlibcX86_64}}).argv
}

// runEntrypointWith executes a branch entrypoint with sh against pgdata in a
// sandbox. Absolute paths the script uses are rewritten into a temporary
// root: /pgoverlay/ (the rw volume, with the lazyrw builds installed as empty
// files) and /lib/ld-musl-* (the libc probe). LD_PRELOAD is renamed so the
// host's own loader never sees the fake builds. mount, umount, chown, uname,
// gosu and postgres are stubs (they need root, or report the host), and the
// stub docker-entrypoint.sh records the postgres argv and LD_PRELOAD instead
// of starting anything.
func runEntrypointWith(t *testing.T, script, pgdata string, o entrypointEnv) entrypointRun {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	out := t.TempDir()
	stubs := t.TempDir()
	if o.arch == "" {
		o.arch = "x86_64"
	}
	if o.selftest == "" {
		o.selftest = "pass"
	}
	if o.builds == nil {
		o.builds = LazyRWVariantNames()
	}
	if err := os.MkdirAll(filepath.Join(root, "pgoverlay/rw/lazyrw"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range o.builds {
		if err := os.WriteFile(filepath.Join(root, "pgoverlay/rw/lazyrw", LazyRWFileName(v)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if o.musl {
		if err := os.WriteFile(filepath.Join(root, "lib/ld-musl-"+o.arch+".so.1"), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	good := make([]string, 0, len(o.good))
	for _, v := range o.good {
		good = append(good, LazyRWFileName(v))
	}
	stubBodies := map[string]string{
		// The self-test mount stands its merged dir in for the lower dir, so
		// a write through it is visible to the fd opened before: what a
		// kernel with stacked file operations shows. Every other mount (the
		// branch's own overlay) is a no-op.
		"mount": `for a in "$@"; do target=$a; done
case $target in
*/.selftest/merged)
  [ "$SELFTEST" = pass ] || { echo "mount: permission denied" >&2; exit 32; }
  for a in "$@"; do case $a in lowerdir=*) lower=${a#lowerdir=}; lower=${lower%%,*} ;; esac; done
  rmdir "$target" && ln -s "$lower" "$target" ;;
esac`,
		"umount": "exit 0",
		"chown":  "exit 0",
		"uname":  `[ "$1" = -m ] && echo "$FAKE_UNAME_M"`,
		"gosu":   `[ "$1" = postgres ] || exit 1; shift; echo gosu >> "$OUT/runas"; exec "$@"`,
		// postgres -V "loads" a preloaded build when it is one of GOOD, and
		// then the shim, active in a process named postgres with an absolute
		// PGDATA, reports itself when PGOVERLAY_LAZYRW_DEBUG=1.
		"postgres": `[ "$1" = -V ] || exit 1
echo "postgres (PostgreSQL) ${FAKE_PG_VERSION:-17.6}"
for so in $(echo "${FAKE_LD_PRELOAD:-}" | tr ':' ' '); do
  for g in $GOOD; do
    case $so in */"$g")
      [ "${PGOVERLAY_LAZYRW_DEBUG:-}" = 1 ] && echo "[lazyrw] active pid=$$ PGDATA=$PGDATA" >&2 ;;
    esac
  done
done
exit 0`,
		"docker-entrypoint.sh": `for a in "$@"; do printf '%s\n' "$a"; done > "$OUT/args"
printf '%s' "${FAKE_LD_PRELOAD:-}" > "$OUT/preload"`,
	}
	if o.noRunas {
		delete(stubBodies, "gosu")
	}
	for name, body := range stubBodies {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sandboxed := strings.NewReplacer(
		"/pgoverlay/", root+"/pgoverlay/",
		"/lib/ld-musl-", root+"/lib/ld-musl-",
		"LD_PRELOAD", "FAKE_LD_PRELOAD",
	).Replace(script)
	scriptPath := filepath.Join(t.TempDir(), "entrypoint.sh")
	if err := os.WriteFile(scriptPath, []byte(sandboxed), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the stubs and the system directories, so a gosu or postgres on
	// the host's PATH cannot take part.
	cmd := exec.Command(sh, scriptPath)
	cmd.Env = append([]string{
		"PATH=" + stubs + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"HOME=" + t.TempDir(),
		"PGDATA=" + pgdata, "PGOVERLAY_LOWERS=/pgoverlay/lower/0",
		"OUT=" + out, "SELFTEST=" + o.selftest, "FAKE_UNAME_M=" + o.arch,
		"GOOD=" + strings.Join(good, " "),
	}, o.env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("entrypoint: %v\n%s", err, stderr.String())
	}
	run := entrypointRun{stderr: stderr.String(), root: root, out: out}
	b, err := os.ReadFile(filepath.Join(out, "args"))
	if err != nil {
		t.Fatal(err)
	}
	run.argv = strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if b, err := os.ReadFile(filepath.Join(out, "preload")); err == nil {
		run.preload = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(root, "pgoverlay/rw/cow-mode")); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l != "" {
				run.cowMode = append(run.cowMode, l)
			}
		}
	}
	return run
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

// pgdataWith returns a data dir whose PG_VERSION says major.
func pgdataWith(t *testing.T, major string) string {
	t.Helper()
	pgdata := t.TempDir()
	if err := os.WriteFile(filepath.Join(pgdata, "PG_VERSION"), []byte(major+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return pgdata
}

// The overlay entrypoint preloads the build for the image's libc and
// architecture, records lazyrw in cow-mode and says so in the log. The probe
// runs postgres as the postgres user when the image has gosu.
func TestEntrypointLazyRWPicksTheImagesBuild(t *testing.T) {
	cases := []struct {
		arch    string
		musl    bool
		variant string
	}{
		{"x86_64", false, LazyRWGlibcX86_64},
		{"aarch64", false, LazyRWGlibcAarch64},
		{"x86_64", true, LazyRWMuslX86_64},
		{"aarch64", true, LazyRWMuslAarch64},
	}
	for _, c := range cases {
		t.Run(c.variant, func(t *testing.T) {
			run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "17"), entrypointEnv{
				arch: c.arch, musl: c.musl, good: []string{c.variant},
			})
			so := filepath.Join(run.root, "pgoverlay/rw/lazyrw", LazyRWFileName(c.variant))
			if run.preload != so {
				t.Errorf("LD_PRELOAD = %q, want %q", run.preload, so)
			}
			if want := []string{CowModeLazyRW, so}; !slices.Equal(run.cowMode, want) {
				t.Errorf("cow-mode = %q, want %q", run.cowMode, want)
			}
			if !strings.Contains(run.stderr, "pgoverlay: lazyrw active") {
				t.Errorf("stderr does not report lazyrw active:\n%s", run.stderr)
			}
			if _, ok := settings(t, run.argv)["io_method"]; ok {
				t.Errorf("io_method set on PG 17: %q", run.argv)
			}
			if _, err := os.Stat(filepath.Join(run.root, "pgoverlay/rw/.selftest")); !os.IsNotExist(err) {
				t.Errorf("the self-test left its scratch dir behind (stat err %v)", err)
			}
		})
	}
}

// A preload list the image already sets is kept, after the shim.
func TestEntrypointLazyRWKeepsTheImagesPreload(t *testing.T) {
	run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "17"), entrypointEnv{
		good: []string{LazyRWGlibcX86_64}, env: []string{"FAKE_LD_PRELOAD=/usr/lib/libjemalloc.so.2"},
	})
	so := filepath.Join(run.root, "pgoverlay/rw/lazyrw", LazyRWFileName(LazyRWGlibcX86_64))
	if want := so + ":/usr/lib/libjemalloc.so.2"; run.preload != want {
		t.Fatalf("LD_PRELOAD = %q, want %q", run.preload, want)
	}
}

// A musl loader next to a glibc postgres (or the reverse) must not leave the
// branch eager: the other libc's build is tried next.
func TestEntrypointLazyRWFallsBackToTheOtherLibc(t *testing.T) {
	run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "17"), entrypointEnv{
		musl: true, good: []string{LazyRWGlibcX86_64},
	})
	so := filepath.Join(run.root, "pgoverlay/rw/lazyrw", LazyRWFileName(LazyRWGlibcX86_64))
	if run.preload != so || len(run.cowMode) == 0 || run.cowMode[0] != CowModeLazyRW {
		t.Fatalf("LD_PRELOAD = %q, cow-mode = %q; want the glibc build, lazyrw", run.preload, run.cowMode)
	}
}

// Whatever keeps the shim from being safe or from loading, the branch still
// starts, copies eagerly (no LD_PRELOAD) and says why in cow-mode and in a
// WARN line.
func TestEntrypointLazyRWFallsBackToEager(t *testing.T) {
	cases := []struct {
		name string
		o    entrypointEnv
		why  string
	}{
		{"kernel self-test fails", entrypointEnv{selftest: "fail", good: []string{LazyRWGlibcX86_64}}, "self-test failed"},
		{"no build for the architecture", entrypointEnv{arch: "riscv64", good: []string{LazyRWGlibcX86_64}}, "no lazyrw build for glibc-riscv64"},
		{"build missing from the volume", entrypointEnv{builds: []string{}, good: []string{LazyRWGlibcX86_64}}, "no lazyrw build for glibc-x86_64"},
		{"build does not load", entrypointEnv{good: nil}, "liblazyrw-glibc-x86_64.so did not load into postgres"},
		{"build does not load without gosu", entrypointEnv{good: nil, noRunas: true}, "did not load into postgres"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "18"), c.o)
			if run.preload != "" {
				t.Errorf("LD_PRELOAD = %q, want none", run.preload)
			}
			if len(run.cowMode) != 2 || run.cowMode[0] != CowModeEager || !strings.Contains(run.cowMode[1], c.why) {
				t.Errorf("cow-mode = %q, want eager and a detail containing %q", run.cowMode, c.why)
			}
			if !strings.Contains(run.stderr, "pgoverlay: WARN: lazyrw is not active") || !strings.Contains(run.stderr, c.why) {
				t.Errorf("stderr lacks the WARN with %q:\n%s", c.why, run.stderr)
			}
			if _, ok := settings(t, run.argv)["io_method"]; ok {
				t.Errorf("io_method pinned although the shim is not active: %q", run.argv)
			}
		})
	}
}

// PGOVERLAY_LAZYRW=off skips the shim entirely: no self-test, no preload.
func TestEntrypointLazyRWOff(t *testing.T) {
	run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "18"), entrypointEnv{
		env: []string{"PGOVERLAY_LAZYRW=off"}, selftest: "fail", good: []string{LazyRWGlibcX86_64},
	})
	if run.preload != "" {
		t.Errorf("LD_PRELOAD = %q with PGOVERLAY_LAZYRW=off", run.preload)
	}
	if want := []string{CowModeOff, "PGOVERLAY_LAZYRW=off"}; !slices.Equal(run.cowMode, want) {
		t.Errorf("cow-mode = %q, want %q", run.cowMode, want)
	}
	if strings.Contains(run.stderr, "WARN") {
		t.Errorf("off is a choice, not a failure; stderr:\n%s", run.stderr)
	}
	if _, ok := settings(t, run.argv)["io_method"]; ok {
		t.Errorf("io_method pinned with the shim off: %q", run.argv)
	}
}

// PG 18 and later get -c io_method=worker, once, when the shim is active: the
// major comes from PG_VERSION, else the image's PG_MAJOR, else postgres -V.
func TestEntrypointPinsIOMethodWorkerFromPG18(t *testing.T) {
	cases := []struct {
		name       string
		pgVersion  string // PG_VERSION content; "-" = no file
		env        []string
		wantWorker bool
	}{
		{"PG_VERSION 18", "18", nil, true},
		{"PG_VERSION 19", "19", nil, true},
		{"PG_VERSION 17", "17", []string{"PG_MAJOR=18"}, false},
		{"PG_VERSION 14", "14", nil, false},
		{"PG_MAJOR 18", "-", []string{"PG_MAJOR=18"}, true},
		{"PG_MAJOR 16", "-", []string{"PG_MAJOR=16"}, false},
		{"postgres -V 18", "-", []string{"FAKE_PG_VERSION=18.1"}, true},
		{"postgres -V 17", "-", []string{"FAKE_PG_VERSION=17.6"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pgdata := t.TempDir()
			if c.pgVersion != "-" {
				pgdata = pgdataWith(t, c.pgVersion)
			}
			run := runEntrypointWith(t, EntrypointScript, pgdata, entrypointEnv{env: c.env, good: []string{LazyRWGlibcX86_64}})
			got, ok := settings(t, run.argv)["io_method"]
			if ok != c.wantWorker || (ok && got != "worker") {
				t.Fatalf("io_method = %q (set %v), want worker %v; argv %q", got, ok, c.wantWorker, run.argv)
			}
			n := 0
			for _, a := range run.argv {
				if strings.HasPrefix(a, "io_method=") {
					n++
				}
			}
			if ok && n != 1 {
				t.Errorf("io_method given %d times: %q", n, run.argv)
			}
		})
	}
}

// branchd --wal-recycle=off reaches postgres as -c wal_recycle=off; the
// default leaves recycling as configured.
func TestEntrypointWALRecycle(t *testing.T) {
	for _, c := range []struct {
		env  []string
		want bool
	}{{nil, false}, {[]string{"PGOVERLAY_WAL_RECYCLE=on"}, false}, {[]string{"PGOVERLAY_WAL_RECYCLE=off"}, true}} {
		run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "17"), entrypointEnv{env: c.env, good: []string{LazyRWGlibcX86_64}})
		got, ok := settings(t, run.argv)["wal_recycle"]
		if ok != c.want || (ok && got != "off") {
			t.Errorf("env %q: wal_recycle = %q (set %v), want off %v", c.env, got, ok, c.want)
		}
	}
}

// The probe runs postgres as the postgres user when the image has gosu (the
// real start drops to it the same way, so the build must be readable by it).
func TestEntrypointLazyRWProbeRunsAsPostgres(t *testing.T) {
	run := runEntrypointWith(t, EntrypointScript, pgdataWith(t, "17"), entrypointEnv{good: []string{LazyRWGlibcX86_64}})
	if len(run.cowMode) == 0 || run.cowMode[0] != CowModeLazyRW {
		t.Fatalf("cow-mode = %q", run.cowMode)
	}
	if b, err := os.ReadFile(filepath.Join(run.out, "runas")); err != nil || strings.TrimSpace(string(b)) != "gosu" {
		t.Fatalf("the probe did not run through gosu postgres (runas log %q, err %v)", b, err)
	}
}

// The two entrypoints share their tail (see the "shared with" marker): the
// same config fixes and the same postgres command line, with the
// backend-specific settings in "$@".
func TestEntrypointsShareTheirTail(t *testing.T) {
	section := func(script, from, to string) string {
		t.Helper()
		i := strings.Index(script, from)
		j := strings.Index(script, to)
		if i < 0 || j < i {
			t.Fatalf("markers %q .. %q not found in order", from, to)
		}
		return script[i : j+len(to)]
	}
	const ssl = `[ -e "$PGDATA/server.crt" ] || set -- "$@" -c ssl=off`
	a := section(EntrypointScript, "# A branch is always an independent", ssl)
	b := section(EntrypointScriptDirect, "# A branch is always an independent", ssl)
	if a != b {
		t.Errorf("the shared config fixes differ:\n--- overlay\n%s\n--- direct\n%s", a, b)
	}
	a = EntrypointScript[strings.Index(EntrypointScript, "exec docker-entrypoint.sh"):]
	b = EntrypointScriptDirect[strings.Index(EntrypointScriptDirect, "exec docker-entrypoint.sh"):]
	if a != b {
		t.Errorf("the postgres command lines differ:\n--- overlay\n%s\n--- direct\n%s", a, b)
	}
	for name, s := range entrypoints {
		// "$@" collects the extra settings from the top of the script on
		if strings.Count(s, "\nset --\n") != 1 || strings.Index(s, "\nset --\n") > strings.Index(s, "--- shared with") {
			t.Errorf("%s: want one `set --`, before the shared tail", name)
		}
	}
}
