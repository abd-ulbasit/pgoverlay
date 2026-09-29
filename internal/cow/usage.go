package cow

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
)

// usageDist carries the static pgoverlay-du binaries (usage/pgoverlay-du.c)
// when this build has them; see usage/dist/README.md. The directory always
// holds at least its README, so the embed compiles without the binaries.
//
//go:embed usage/dist
var usageDist embed.FS

// duDist is where DuTool looks for the binaries; tests swap in their own.
var duDist fs.FS = usageDist

const duDistDir = "usage/dist"

// MaxDuToolSize is the largest pgoverlay-du binary DuTool hands out. The
// binary reaches the helper base64-encoded in environment variables, and a
// kube helper's environment travels in a Secret (1 MiB at most); a musl static
// build of the tool is well under 100 KiB.
const MaxDuToolSize = 256 << 10

// duEnvChunk is the most base64 text one environment variable carries. Linux
// refuses any single argv or environment string over MAX_ARG_STRLEN (32
// pages, 128 KiB) at exec, so the encoded binary is split well below that.
const duEnvChunk = 96 << 10

// NormalizeMachine maps an architecture name as `uname -m` or Go spells it
// onto the name the pgoverlay-du binaries use ("" when it is not one pgoverlay
// builds for).
func NormalizeMachine(m string) string {
	switch strings.TrimSpace(m) {
	case "x86_64", "amd64":
		return "x86_64"
	case "aarch64", "arm64", "armv8l":
		return "aarch64"
	}
	return ""
}

// DuTool returns the embedded static pgoverlay-du binary for machine (as
// `uname -m` prints it), or nil when this build carries none for it. A binary
// larger than MaxDuToolSize, or one that does not match its SHA256SUMS line
// (when the file lists it), is treated as missing.
func DuTool(machine string) []byte {
	m := NormalizeMachine(machine)
	if m == "" {
		return nil
	}
	name := "pgoverlay-du-" + m
	bin, err := fs.ReadFile(duDist, path.Join(duDistDir, name))
	if err != nil || len(bin) == 0 || len(bin) > MaxDuToolSize {
		return nil
	}
	if sums, err := fs.ReadFile(duDist, path.Join(duDistDir, "SHA256SUMS")); err == nil {
		want, listed := sha256Listed(sums, name)
		if !listed {
			return nil
		}
		got := sha256.Sum256(bin)
		if hex.EncodeToString(got[:]) != want {
			return nil
		}
	}
	return bin
}

// DuTools returns every embedded pgoverlay-du binary, by machine name.
func DuTools() map[string][]byte {
	out := map[string][]byte{}
	for _, m := range []string{"x86_64", "aarch64"} {
		if bin := DuTool(m); bin != nil {
			out[m] = bin
		}
	}
	return out
}

// sha256Listed finds name's digest in sha256sum output ("<hex>  <name>" or
// "<hex> *<name>").
func sha256Listed(sums []byte, name string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name || path.Base(strings.TrimPrefix(fields[1], "*")) == name {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// duToolEnv encodes bin into environment variables <prefix>0, <prefix>1, ...
// and returns them with the shell words ("$<prefix>0" "$<prefix>1" ...) that
// expand back to the encoded text in order.
func duToolEnv(prefix string, bin []byte) (env []string, words string) {
	enc := base64.StdEncoding.EncodeToString(bin)
	var w []string
	for i := 0; len(enc) > 0; i++ {
		n := min(len(enc), duEnvChunk)
		name := prefix + strconv.Itoa(i)
		env = append(env, name+"="+enc[:n])
		w = append(w, `"$`+name+`"`)
		enc = enc[n:]
	}
	return env, strings.Join(w, " ")
}

// DuToolPath is where the helpers install the pgoverlay-du binary.
const DuToolPath = "/tmp/pgoverlay-du"

// duInstall is the shell that writes the binary encoded in words to
// DuToolPath and makes it executable.
func duInstall(words string) string {
	return fmt.Sprintf("printf '%%s' %s | base64 -d > %s\nchmod 0755 %s\n", words, DuToolPath, DuToolPath)
}

// DuUsageCommand returns the helper command and environment that print
// pgoverlay-du's totals for dir (the helper must mount the volume there):
// "<exclusive>\t<shared>\t<apparent>\t<dir>", which parses like `du -sb`.
func DuUsageCommand(bin []byte, dir string) (cmd, env []string) {
	env, words := duToolEnv("PGOVERLAY_DU_", bin)
	script := "set -eu\n" + duInstall(words) + `exec ` + DuToolPath + ` -- "$1"` + "\n"
	return []string{"sh", "-c", script, "pgoverlay-du", dir}, env
}

// DuCowExtSizeCommand returns the helper command and environment that set the
// XFS copy-on-write extent size hint of dir to bytes (0 clears it) and print
// "cowextsize=<bytes>\t<dir>" as the filesystem reports it back.
func DuCowExtSizeCommand(bin []byte, dir string, bytes int64) (cmd, env []string) {
	env, words := duToolEnv("PGOVERLAY_DU_", bin)
	script := "set -eu\n" + duInstall(words) + `exec ` + DuToolPath + ` -c "$1" "$2"` + "\n"
	return []string{"sh", "-c", script, "pgoverlay-du", strconv.FormatInt(bytes, 10), dir}, env
}

// DuTotals is one line of pgoverlay-du output.
type DuTotals struct {
	Exclusive int64 // bytes in extents no other file shares: what the tree costs on its own
	Shared    int64 // bytes in extents shared with other files (reflink clones)
	Apparent  int64 // sum of file sizes, what `du -sb` reports
}

// ParseDuTotals reads the first totals line pgoverlay-du printed.
func ParseDuTotals(out string) (DuTotals, error) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) < 4 {
			continue
		}
		var t DuTotals
		var err error
		if t.Exclusive, err = strconv.ParseInt(f[0], 10, 64); err != nil {
			continue
		}
		if t.Shared, err = strconv.ParseInt(f[1], 10, 64); err != nil {
			continue
		}
		if t.Apparent, err = strconv.ParseInt(f[2], 10, 64); err != nil {
			continue
		}
		return t, nil
	}
	return DuTotals{}, fmt.Errorf("no pgoverlay-du totals in %q", out)
}

// ParseCowExtSize reads the hint pgoverlay-du -c reports back.
func ParseCowExtSize(out string) (int64, error) {
	for _, line := range strings.Split(out, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(line), "cowextsize=")
		if !ok {
			continue
		}
		v, _, _ = strings.Cut(v, "\t")
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, fmt.Errorf("no cowextsize in %q", out)
}
