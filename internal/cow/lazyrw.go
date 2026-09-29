package cow

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// The lazyrw LD_PRELOAD shim (lazyrw/lazyrw.c): Postgres in a branch opens
// relation files read-only until its first write to each one, so OverlayFS
// copies a file up when it is written instead of when it is read.
//
// It is prebuilt for every libc and architecture a branch image can have and
// committed, so `go install` needs no C toolchain. hack/build-lazyrw.sh
// builds the variants reproducibly; CI rebuilds them from source and fails on
// any byte of difference.
//
//go:embed lazyrw/dist/*.so lazyrw/dist/SHA256SUMS
var lazyrwDist embed.FS

const lazyrwDir = "lazyrw/dist"

// LazyRW variant names, "<libc>-<uname -m>": the branch entrypoint picks one
// from `uname -m` and whether the image's libc is musl.
const (
	LazyRWGlibcX86_64  = "glibc-x86_64"
	LazyRWGlibcAarch64 = "glibc-aarch64"
	LazyRWMuslX86_64   = "musl-x86_64"
	LazyRWMuslAarch64  = "musl-aarch64"
)

// LazyRWFileName is the file name a variant is built and installed under,
// "liblazyrw-<variant>.so".
func LazyRWFileName(variant string) string { return "liblazyrw-" + variant + ".so" }

// Variants returns every embedded shim build keyed by variant name
// (LazyRWGlibcX86_64, ...). The slices are fresh copies on each call.
func Variants() map[string][]byte {
	out := make(map[string][]byte, 4)
	entries, err := fs.ReadDir(lazyrwDist, lazyrwDir)
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "liblazyrw-") || !strings.HasSuffix(name, ".so") {
			continue
		}
		b, err := lazyrwDist.ReadFile(path.Join(lazyrwDir, name))
		if err != nil {
			panic(err)
		}
		out[strings.TrimSuffix(strings.TrimPrefix(name, "liblazyrw-"), ".so")] = b
	}
	return out
}

// LazyRWVariantNames returns the embedded variant names, sorted.
func LazyRWVariantNames() []string {
	v := Variants()
	names := make([]string, 0, len(v))
	for n := range v {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// VerifyLazyRW checks every embedded build against the embedded SHA256SUMS
// and that SHA256SUMS lists no shim build that is not embedded.
func VerifyLazyRW() error {
	sums, err := lazyrwDist.ReadFile(path.Join(lazyrwDir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	want := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			return fmt.Errorf("lazyrw SHA256SUMS: malformed line %q", sc.Text())
		}
		want[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	if err := sc.Err(); err != nil {
		return err
	}
	for v, b := range Variants() {
		name := LazyRWFileName(v)
		sum := sha256.Sum256(b)
		got := hex.EncodeToString(sum[:])
		switch w, ok := want[name]; {
		case !ok:
			return fmt.Errorf("lazyrw: %s is not in SHA256SUMS", name)
		case w != got:
			return fmt.Errorf("lazyrw: %s has sha256 %s, SHA256SUMS says %s", name, got, w)
		}
		delete(want, name)
	}
	for name := range want {
		if strings.HasPrefix(name, "liblazyrw-") {
			return fmt.Errorf("lazyrw: SHA256SUMS lists %s, which is not embedded", name)
		}
	}
	return nil
}
