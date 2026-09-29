package cow

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// LazyRWDir is where the overlay install puts the lazyrw builds: a directory
// of the branch's rw volume, outside upper/, so it is never overlay content
// (and a frozen layer's copy is never read).
const LazyRWDir = RWPath + "/lazyrw"

// CowModePath is the file the overlay entrypoint writes on every start: the
// copy-on-write mode the branch's Postgres runs in (CowModeLazyRW,
// CowModeEager or CowModeOff) on the first line and a detail on the second.
const CowModePath = RWPath + "/cow-mode"

// The modes the overlay entrypoint reports in CowModePath.
const (
	// CowModeLazyRW: the lazyrw shim is active. A read copies nothing; the
	// first write to a relation file copies that file into the branch.
	CowModeLazyRW = "lazyrw"
	// CowModeEager: the shim was asked for but is not active (the kernel
	// self-test or the preload probe failed), so every relation file
	// Postgres opens is copied into the branch.
	CowModeEager = "eager"
	// CowModeOff: the shim is switched off (PGOVERLAY_LAZYRW=off); the
	// branch copies eagerly by choice.
	CowModeOff = "off"
)

// LazyRWEnvName is the environment variable that carries a variant's build,
// base64-encoded, to the install helper: PGOVERLAY_LAZYRW_<VARIANT> with the
// variant upper-cased and its dash as an underscore.
func LazyRWEnvName(variant string) string {
	return "PGOVERLAY_LAZYRW_" + strings.ToUpper(strings.ReplaceAll(variant, "-", "_"))
}

// overlayInstall is built once: the builds are embedded and never change.
var overlayInstall = sync.OnceValues(func() (string, []string) {
	var sh strings.Builder
	sh.WriteString(`set -eu
rw=` + RWPath + `
printf '%s' "$PGOVERLAY_ENTRYPOINT" > "$rw/entrypoint.sh"
chmod 0755 "$rw/entrypoint.sh"
mkdir -p "$rw/upper" "$rw/work" "$rw/lazyrw"
# install_so NAME SHA256 BASE64: decode, check and atomically place one build.
# Postgres loads it as its own user, so it is world-readable.
install_so() {
  printf '%s' "$3" | base64 -d > "$rw/lazyrw/$1.tmp"
  sum=$(sha256sum "$rw/lazyrw/$1.tmp")
  if [ "${sum%% *}" != "$2" ]; then
    rm -f "$rw/lazyrw/$1.tmp"
    echo "install: $1 arrived damaged: sha256 ${sum%% *}, want $2" >&2
    exit 1
  fi
  chmod 0644 "$rw/lazyrw/$1.tmp"
  mv -f "$rw/lazyrw/$1.tmp" "$rw/lazyrw/$1"
}
`)
	builds := Variants()
	env := make([]string, 0, len(builds)+1)
	env = append(env, "PGOVERLAY_ENTRYPOINT="+EntrypointScript)
	for _, v := range LazyRWVariantNames() {
		b := builds[v]
		sum := sha256.Sum256(b)
		fmt.Fprintf(&sh, "install_so %s %s \"$%s\"\n", LazyRWFileName(v), hex.EncodeToString(sum[:]), LazyRWEnvName(v))
		env = append(env, LazyRWEnvName(v)+"="+base64.StdEncoding.EncodeToString(b))
	}
	sh.WriteString(`chmod 0755 "$rw/lazyrw"` + "\n")
	return sh.String(), env
})

// OverlayInstall returns the command and environment of the helper that
// prepares an overlay branch's rw volume, mounted at RWPath: it writes the
// entrypoint, creates upper/ and work/, and installs every lazyrw build into
// LazyRWDir, each passed base64-encoded in its own environment variable
// (LazyRWEnvName) and checked against its SHA-256 after decoding. Every build
// is installed, whatever the branch image: the entrypoint picks the one that
// matches the image's libc and architecture. Rerunning it on a volume that
// already has them (recover) replaces them in place. It needs a POSIX shell
// with base64 and sha256sum (busybox has both), which the runtime's utility
// image and any --kube-helper-image mirror of it provide. The environment is
// shared: callers must not modify it.
func OverlayInstall() (cmd, env []string) {
	sh, env := overlayInstall()
	return []string{"sh", "-c", sh}, env
}
