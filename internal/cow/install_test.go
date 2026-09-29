package cow

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runInstall runs the overlay install helper's script with sh against a
// temporary directory standing in for the rw volume, with env as its
// environment, and returns that directory.
func runInstall(t *testing.T, env []string) (string, error) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"base64", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s (the helper image has it)", tool)
		}
	}
	cmd, _ := OverlayInstall()
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("install cmd = %q, want sh -c SCRIPT", cmd)
	}
	rw := t.TempDir()
	script := strings.ReplaceAll(cmd[2], "rw="+RWPath+"\n", "rw="+rw+"\n")
	if script == cmd[2] {
		t.Fatal("the install script does not set rw=" + RWPath)
	}
	c := exec.Command(sh, "-c", script)
	c.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := c.CombinedOutput()
	if err != nil {
		return rw, &exec.ExitError{ProcessState: c.ProcessState, Stderr: out}
	}
	return rw, nil
}

// The install helper writes the entrypoint and every lazyrw build, byte for
// byte, into the rw volume, readable by the postgres user.
func TestOverlayInstallWritesEntrypointAndBuilds(t *testing.T) {
	_, env := OverlayInstall()
	rw, err := runInstall(t, env)
	if err != nil {
		t.Fatalf("install: %v: %s", err, err.(*exec.ExitError).Stderr)
	}
	ep, err := os.ReadFile(filepath.Join(rw, "entrypoint.sh"))
	if err != nil || string(ep) != EntrypointScript {
		t.Fatalf("entrypoint.sh differs from EntrypointScript (err %v)", err)
	}
	if fi, err := os.Stat(filepath.Join(rw, "entrypoint.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("entrypoint.sh mode = %v (err %v), want 0755", fi.Mode().Perm(), err)
	}
	for _, d := range []string{"upper", "work", "lazyrw"} {
		if fi, err := os.Stat(filepath.Join(rw, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s/ missing (err %v)", d, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(rw, "lazyrw")); err == nil && fi.Mode().Perm() != 0o755 {
		t.Errorf("lazyrw/ mode = %v, want 0755", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Join(rw, "lazyrw"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(Variants()) {
		t.Errorf("lazyrw/ holds %d files, want the %d builds and nothing else", len(entries), len(Variants()))
	}
	for v, want := range Variants() {
		p := filepath.Join(rw, "lazyrw", LazyRWFileName(v))
		got, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: installed build differs from the embedded one (err %v)", v, err)
			continue
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %v, want 0644", v, fi.Mode().Perm())
		}
	}
	// recover reinstalls over an existing volume
	if _, err := runInstall(t, env); err != nil {
		t.Fatalf("second install: %v", err)
	}
}

// A build that arrives damaged (the environment is its only transport) fails
// the install instead of being preloaded into Postgres.
func TestOverlayInstallRejectsADamagedBuild(t *testing.T) {
	_, env := OverlayInstall()
	damaged := append([]string(nil), env...)
	key := LazyRWEnvName(LazyRWGlibcX86_64) + "="
	found := false
	for i, e := range damaged {
		if strings.HasPrefix(e, key) {
			v := []byte(strings.TrimPrefix(e, key))
			// another base64 digit: still decodes, to different bytes
			if v[100] == 'A' {
				v[100] = 'B'
			} else {
				v[100] = 'A'
			}
			damaged[i] = key + string(v)
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s in the install environment", key)
	}
	rw, err := runInstall(t, damaged)
	if err == nil {
		t.Fatal("install accepted a build whose sha256 does not match")
	}
	if msg := string(err.(*exec.ExitError).Stderr); !strings.Contains(msg, LazyRWFileName(LazyRWGlibcX86_64)+" arrived damaged") {
		t.Errorf("install failed, but not on the checksum: %s", msg)
	}
	if _, err := os.Stat(filepath.Join(rw, "lazyrw", LazyRWFileName(LazyRWGlibcX86_64))); !os.IsNotExist(err) {
		t.Errorf("the damaged build was installed (stat err %v)", err)
	}
}

// Each build rides in its own variable, named after its variant, and the
// whole environment fits the limits the runtimes put on it: one string under
// MAX_ARG_STRLEN, all of them well under a kube Secret's 1 MiB.
func TestOverlayInstallEnvironment(t *testing.T) {
	_, env := OverlayInstall()
	names := map[string]bool{}
	total := 0
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		names[k] = true
		total += len(e)
		if len(e) >= 32*4096 {
			t.Errorf("%s is %d bytes, over MAX_ARG_STRLEN", k, len(e))
		}
	}
	want := []string{"PGOVERLAY_ENTRYPOINT", "PGOVERLAY_LAZYRW_GLIBC_X86_64", "PGOVERLAY_LAZYRW_GLIBC_AARCH64",
		"PGOVERLAY_LAZYRW_MUSL_X86_64", "PGOVERLAY_LAZYRW_MUSL_AARCH64"}
	for _, k := range want {
		if !names[k] {
			t.Errorf("install environment lacks %s", k)
		}
	}
	if len(names) != len(want) {
		t.Errorf("install environment has %v, want exactly %v", names, want)
	}
	if total > 512<<10 {
		t.Errorf("install environment is %d bytes; keep it well under a kube Secret's 1 MiB", total)
	}
}

// shellcheck (when installed; CI's runners have it) finds nothing in the
// scripts pgoverlay runs inside containers.
func TestShellcheckCowScripts(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck not installed")
	}
	installCmd, _ := OverlayInstall()
	scripts := map[string]string{
		"entrypoint.sh":        EntrypointScript,
		"entrypoint_direct.sh": EntrypointScriptDirect,
		"install.sh":           "#!/bin/sh\n" + installCmd[2],
	}
	for name, body := range scripts {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(sc, "-s", "sh", p).CombinedOutput(); err != nil {
			t.Errorf("shellcheck %s: %v\n%s", name, err, out)
		}
	}
}

// The entrypoint's preload probe recognises the shim by the line it prints
// when it activates under PGOVERLAY_LAZYRW_DEBUG=1. Rewording that line in
// lazyrw.c would turn lazyrw off in every branch, so it is pinned here.
func TestLazyRWActiveLineMatchesTheProbe(t *testing.T) {
	src, err := os.ReadFile("lazyrw/lazyrw.c")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`dbg\("\[lazyrw\] active pid=`).Match(src) {
		t.Fatal(`lazyrw.c no longer prints "[lazyrw] active pid=..." when it activates; entrypoint.sh's lazyrw_probe matches it`)
	}
	if !strings.Contains(EntrypointScript, `grep -q '^\[lazyrw\] active '`) {
		t.Fatal("entrypoint.sh's probe no longer matches the shim's active line")
	}
}
