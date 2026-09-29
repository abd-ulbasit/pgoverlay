package cow

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func withDuDist(t *testing.T, files fstest.MapFS) {
	t.Helper()
	old := duDist
	t.Cleanup(func() { duDist = old })
	duDist = files
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestNormalizeMachine(t *testing.T) {
	for in, want := range map[string]string{
		"x86_64": "x86_64", "amd64": "x86_64", "aarch64": "aarch64", "arm64": "aarch64",
		" x86_64\n": "x86_64", "riscv64": "", "": "", "i686": "",
	} {
		if got := NormalizeMachine(in); got != want {
			t.Errorf("NormalizeMachine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDuToolSelection(t *testing.T) {
	x86, arm := []byte("\x7fELF x86 tool"), []byte("\x7fELF arm tool")
	big := make([]byte, MaxDuToolSize+1)
	cases := []struct {
		name    string
		files   fstest.MapFS
		machine string
		want    []byte
	}{
		{"no binaries: README only", fstest.MapFS{"usage/dist/README.md": {Data: []byte("x")}}, "x86_64", nil},
		{"binary without SHA256SUMS", fstest.MapFS{"usage/dist/pgoverlay-du-x86_64": {Data: x86}}, "x86_64", x86},
		{"go arch name", fstest.MapFS{"usage/dist/pgoverlay-du-aarch64": {Data: arm}}, "arm64", arm},
		{"other machine missing", fstest.MapFS{"usage/dist/pgoverlay-du-x86_64": {Data: x86}}, "aarch64", nil},
		{"unknown machine", fstest.MapFS{"usage/dist/pgoverlay-du-x86_64": {Data: x86}}, "s390x", nil},
		{"matching SHA256SUMS", fstest.MapFS{
			"usage/dist/pgoverlay-du-x86_64": {Data: x86},
			"usage/dist/SHA256SUMS":          {Data: []byte(sum(x86) + "  pgoverlay-du-x86_64\n" + sum(arm) + "  pgoverlay-du-aarch64\n")},
		}, "x86_64", x86},
		{"binary-mode SHA256SUMS line", fstest.MapFS{
			"usage/dist/pgoverlay-du-x86_64": {Data: x86},
			"usage/dist/SHA256SUMS":          {Data: []byte(sum(x86) + " *pgoverlay-du-x86_64\n")},
		}, "x86_64", x86},
		{"mismatching SHA256SUMS", fstest.MapFS{
			"usage/dist/pgoverlay-du-x86_64": {Data: x86},
			"usage/dist/SHA256SUMS":          {Data: []byte(sum(arm) + "  pgoverlay-du-x86_64\n")},
		}, "x86_64", nil},
		{"not listed in SHA256SUMS", fstest.MapFS{
			"usage/dist/pgoverlay-du-x86_64": {Data: x86},
			"usage/dist/SHA256SUMS":          {Data: []byte(sum(arm) + "  pgoverlay-du-aarch64\n")},
		}, "x86_64", nil},
		{"too large", fstest.MapFS{"usage/dist/pgoverlay-du-x86_64": {Data: big}}, "x86_64", nil},
		{"empty", fstest.MapFS{"usage/dist/pgoverlay-du-x86_64": {Data: []byte{}}}, "x86_64", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDuDist(t, tc.files)
			if got := DuTool(tc.machine); string(got) != string(tc.want) {
				t.Fatalf("DuTool(%q) = %q, want %q", tc.machine, got, tc.want)
			}
		})
	}
}

func TestDuToolsListsWhatIsEmbedded(t *testing.T) {
	x86 := []byte("x86 tool")
	withDuDist(t, fstest.MapFS{"usage/dist/pgoverlay-du-x86_64": {Data: x86}})
	got := DuTools()
	if len(got) != 1 || string(got["x86_64"]) != string(x86) {
		t.Fatalf("DuTools() = %v", got)
	}
}

// Whatever this build embeds must be usable: every binary in usage/dist is
// handed out (size cap, SHA256SUMS), so a committed binary that DuTool would
// silently ignore fails here instead of in production.
func TestEmbeddedDuToolsAreUsable(t *testing.T) {
	entries, err := fs.ReadDir(usageDist, duDistDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		m, ok := strings.CutPrefix(e.Name(), "pgoverlay-du-")
		if !ok {
			continue
		}
		if DuTool(m) == nil {
			t.Errorf("embedded %s is not usable: over %d bytes, or not matching SHA256SUMS", e.Name(), MaxDuToolSize)
		}
	}
}

func TestDuToolEnvSplitsUnderTheArgLimit(t *testing.T) {
	bin := make([]byte, 200<<10) // 200 KiB -> ~267 KiB of base64, three chunks
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	env, words := duToolEnv("PGOVERLAY_DU_", bin)
	if len(env) != 3 {
		t.Fatalf("%d env vars, want 3", len(env))
	}
	var joined strings.Builder
	for i, e := range env {
		// MAX_ARG_STRLEN is 128 KiB including the name and the NUL
		if len(e)+1 > 128<<10 {
			t.Fatalf("env var %d is %d bytes", i, len(e))
		}
		_, v, _ := strings.Cut(e, "=")
		joined.WriteString(v)
	}
	dec, err := base64.StdEncoding.DecodeString(joined.String())
	if err != nil || string(dec) != string(bin) {
		t.Fatalf("chunks do not reassemble: %v", err)
	}
	if words != `"$PGOVERLAY_DU_0" "$PGOVERLAY_DU_1" "$PGOVERLAY_DU_2"` {
		t.Fatalf("words = %s", words)
	}
}

// runScript runs a helper command locally with its environment, the tool
// installed under dir instead of DuToolPath.
func runScript(t *testing.T, cmd, env []string, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("base64"); err != nil {
		t.Skip("no base64 here")
	}
	args := append([]string(nil), cmd[1:]...)
	args[1] = strings.ReplaceAll(args[1], DuToolPath, filepath.Join(dir, "pgoverlay-du"))
	c := exec.Command(cmd[0], args...)
	c.Env = append(os.Environ(), env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", cmd, err, out)
	}
	return string(out)
}

// The helper scripts decode the tool from the environment and run it with
// the arguments given; a shell script stands in for the binary.
func TestDuCommandsInstallAndRunTheTool(t *testing.T) {
	fake := []byte("#!/bin/sh\necho \"args: $*\"\nprintf '11\\t22\\t33\\t%s\\n' \"$2\"\nprintf 'cowextsize=16384\\t%s\\n' \"$3\"\n")
	dir := t.TempDir()

	cmd, env := DuUsageCommand(fake, "/pgoverlay/rw")
	if cmd[0] != "sh" || cmd[len(cmd)-1] != "/pgoverlay/rw" {
		t.Fatalf("usage cmd %q", cmd)
	}
	out := runScript(t, cmd, env, dir)
	if !strings.Contains(out, "args: -- /pgoverlay/rw") {
		t.Fatalf("tool ran with the wrong arguments:\n%s", out)
	}
	tot, err := ParseDuTotals(out)
	if err != nil || tot != (DuTotals{Exclusive: 11, Shared: 22, Apparent: 33}) {
		t.Fatalf("totals %+v, %v from\n%s", tot, err, out)
	}

	cmd, env = DuCowExtSizeCommand(fake, "/pgoverlay-root", 16384)
	out = runScript(t, cmd, env, dir)
	if !strings.Contains(out, "args: -c 16384 /pgoverlay-root") {
		t.Fatalf("tool ran with the wrong arguments:\n%s", out)
	}
	if n, err := ParseCowExtSize(out); err != nil || n != 16384 {
		t.Fatalf("cowextsize %d, %v", n, err)
	}
}

func TestParseDuTotals(t *testing.T) {
	tot, err := ParseDuTotals("pgoverlay-du: /x: open: Permission denied\n4096\t8192\t12288\t/pgoverlay/rw\n")
	if err != nil || tot != (DuTotals{4096, 8192, 12288}) {
		t.Fatalf("got %+v, %v", tot, err)
	}
	for _, bad := range []string{"", "123\t/pgoverlay/rw", "a\tb\tc\td", "du: cannot access"} {
		if _, err := ParseDuTotals(bad); err == nil {
			t.Errorf("ParseDuTotals(%q) accepted", bad)
		}
	}
}

func TestParseCowExtSize(t *testing.T) {
	if n, err := ParseCowExtSize("cowextsize=0\t/pgoverlay-root\n"); err != nil || n != 0 {
		t.Fatalf("got %d, %v", n, err)
	}
	if _, err := ParseCowExtSize("FS_IOC_FSSETXATTR: Invalid argument"); err == nil {
		t.Fatal("accepted output without a size")
	}
}
