package cow

import (
	"bytes"
	"debug/elf"
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func TestLazyRWEmbeddedBuildsMatchSHA256SUMS(t *testing.T) {
	if err := VerifyLazyRW(); err != nil {
		t.Fatal(err)
	}
}

func TestLazyRWVariantNames(t *testing.T) {
	want := []string{LazyRWGlibcAarch64, LazyRWGlibcX86_64, LazyRWMuslAarch64, LazyRWMuslX86_64}
	if got := LazyRWVariantNames(); !slices.Equal(got, want) {
		t.Fatalf("variants = %q, want %q", got, want)
	}
	if got := LazyRWFileName(LazyRWMuslX86_64); got != "liblazyrw-musl-x86_64.so" {
		t.Fatalf("LazyRWFileName = %q", got)
	}
}

func TestLazyRWVariantsAreFreshCopies(t *testing.T) {
	a := Variants()[LazyRWGlibcX86_64]
	a[0] = 0
	if Variants()[LazyRWGlibcX86_64][0] != 0x7f {
		t.Fatal("Variants shares its slices between calls")
	}
}

// The symbols a build must export: the entry points Postgres writes and
// manages fds through (the pg-import-audit CI job checks the full list
// against what postgres:14-18 import) and the test hooks.
var lazyrwMustExport = []string{
	"open", "open64", "openat", "openat64", "__open_2", "__open64_2", "__openat_2", "__openat64_2",
	"write", "pwrite", "pwrite64", "writev", "pwritev", "pwritev64", "pwritev2", "pwritev64v2",
	"ftruncate", "ftruncate64", "truncate", "truncate64", "fallocate", "fallocate64",
	"posix_fallocate", "posix_fallocate64", "copy_file_range", "sendfile", "sendfile64", "splice",
	"mmap", "mmap64", "close", "close_range", "closefrom", "dup", "dup2", "dup3", "fcntl", "fcntl64",
	"pgoverlay_lazyrw_state", "pgoverlay_lazyrw_active",
}

func TestLazyRWBuildsAreTheRightELF(t *testing.T) {
	for variant, b := range Variants() {
		t.Run(variant, func(t *testing.T) {
			libc, arch, _ := strings.Cut(variant, "-")
			f, err := elf.NewFile(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if f.Class != elf.ELFCLASS64 || f.Type != elf.ET_DYN {
				t.Fatalf("class %v type %v, want a 64-bit shared object", f.Class, f.Type)
			}
			wantMachine := map[string]elf.Machine{"x86_64": elf.EM_X86_64, "aarch64": elf.EM_AARCH64}[arch]
			if f.Machine != wantMachine {
				t.Fatalf("machine %v, want %v", f.Machine, wantMachine)
			}
			libs, err := f.ImportedLibraries()
			if err != nil {
				t.Fatal(err)
			}
			wantLibc := map[string]string{"glibc": "libc.so.6", "musl": "libc.musl-" + arch + ".so.1"}[libc]
			if !slices.Contains(libs, wantLibc) {
				t.Fatalf("needs %q, want %q among them", libs, wantLibc)
			}
			syms, err := f.DynamicSymbols()
			if err != nil {
				t.Fatal(err)
			}
			exported := map[string]bool{}
			for _, s := range syms {
				if s.Section != elf.SHN_UNDEF && elf.ST_TYPE(s.Info) == elf.STT_FUNC {
					exported[s.Name] = true
				}
			}
			for _, name := range lazyrwMustExport {
				if !exported[name] {
					t.Errorf("does not export %s", name)
				}
			}
			// The install helper receives each build base64-encoded in one
			// environment variable, and Linux caps a single argv/env string
			// at MAX_ARG_STRLEN (32 pages, 128 KiB).
			const maxArgStrlen = 32 * 4096
			env := "PGOVERLAY_LAZYRW_" + strings.ToUpper(strings.ReplaceAll(variant, "-", "_")) + "="
			if n := len(env) + base64.StdEncoding.EncodedLen(len(b)) + 1; n > maxArgStrlen {
				t.Fatalf("%d bytes base64-encoded in one env var, over MAX_ARG_STRLEN (%d)", n, maxArgStrlen)
			}
		})
	}
}
