#!/usr/bin/env bash
# The lazyrw shim hands Postgres read-only fds for relation files and upgrades
# them on the first write-class call it sees. A write through a libc function
# it does not interpose would hit a read-only fd and fail with EBADF. This
# audit reads which write-class functions the postgres server binary of each
# supported image imports and fails unless every one is interposed by the
# committed shim build for that image's libc, or is on the reviewed allowlist.
#
#   hack/pg-import-audit.sh
#
# Lists (internal/cow/lazyrw/audit):
#   write-class.txt  libc functions that write file data, change a file's
#                    size, map it writable, submit asynchronous writes, or
#                    duplicate/close fds (the shim's bookkeeping)
#   allow.txt        write-class functions postgres may import without the
#                    shim interposing them, each with the reason it is safe
#
# The binaries are extracted with a throwaway buildx build, so the images land
# in the builder's cache, not the Docker image store (and a remote engine
# works). Needs Docker with buildx and nm (GNU binutils or llvm-nm).
#
# Environment:
#   PG_AUDIT_IMAGES     images to audit (default postgres:14..18 and 17-alpine)
#   PG_AUDIT_PLATFORMS  platforms to audit (default linux/amd64)
#   BUILDX_BUILDER      as in hack/build-lazyrw.sh (default pgoverlay-lazyrw)
#   NM                  nm to use (default nm)
set -euo pipefail
export LC_ALL=C # sort and comm must agree on the order
cd "$(dirname "$0")/.."

AUDIT=internal/cow/lazyrw/audit
DIST=internal/cow/lazyrw/dist
IMAGES=${PG_AUDIT_IMAGES:-postgres:14 postgres:15 postgres:16 postgres:17 postgres:18 postgres:17-alpine}
PLATFORMS=${PG_AUDIT_PLATFORMS:-linux/amd64}
NM=${NM:-nm}

die() { echo "pg-import-audit: $*" >&2; exit 1; }
command -v "$NM" >/dev/null 2>&1 || die "$NM not found (set NM)"
docker buildx version >/dev/null 2>&1 || die "docker buildx is required"
if [ -z "${BUILDX_BUILDER:-}" ]; then
  export BUILDX_BUILDER=pgoverlay-lazyrw
  docker buildx inspect "$BUILDX_BUILDER" >/dev/null 2>&1 ||
    docker buildx create --name "$BUILDX_BUILDER" --driver docker-container >/dev/null
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/pg-import-audit.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# Strip comments and blank lines; first field only.
list() { sed -e 's/#.*//' "$1" | awk 'NF { print $1 }' | sort -u; }
list "$AUDIT/write-class.txt" > "$tmp/write-class"
list "$AUDIT/allow.txt" > "$tmp/allow"

# Function symbols a shared object defines, without version suffixes.
defined() { "$NM" -D --defined-only "$1" | awk 'NF >= 3 && $2 ~ /^[TWi]$/ { sub(/@.*/, "", $3); print $3 }' | sort -u; }
# Symbols a binary imports, without version suffixes.
imported() { "$NM" -D --undefined-only "$1" | awk '{ s = $NF; sub(/@.*/, "", s); print s }' | sort -u; }

rc=0
for platform in $PLATFORMS; do
  case $platform in
    linux/amd64) arch=x86_64 ;;
    linux/arm64) arch=aarch64 ;;
    *) die "unsupported platform $platform" ;;
  esac
  for image in $IMAGES; do
    libc=glibc
    case $image in *alpine*) libc=musl ;; esac
    so=$DIST/liblazyrw-$libc-$arch.so
    [ -f "$so" ] || die "$so is missing: run 'make lazyrw'"
    defined "$so" > "$tmp/interposed"

    out=$tmp/bin-$arch-${image//[:\/]/_}
    # shellcheck disable=SC2016 # the command substitution runs in the build
    printf 'FROM %s AS src\nRUN cp "$(command -v postgres)" /postgres\nFROM scratch\nCOPY --from=src /postgres /\n' "$image" |
      docker buildx build --quiet --platform "$platform" --output "type=local,dest=$out" - >/dev/null ||
      die "could not extract postgres from $image ($platform)"
    imported "$out/postgres" > "$tmp/imports"
    [ -s "$tmp/imports" ] || die "no dynamic imports read from $image's postgres"

    echo "== $image ($platform, $(basename "$so"))"
    comm -12 "$tmp/imports" "$tmp/write-class" > "$tmp/used"
    while read -r sym; do
      if grep -qx "$sym" "$tmp/interposed"; then
        echo "   ok       $sym"
      elif grep -qx "$sym" "$tmp/allow"; then
        echo "   allowed  $sym: $(awk -v s="$sym" '$1 == s { $1 = ""; sub(/^ +/, ""); print; exit }' "$AUDIT/allow.txt")"
      else
        echo "   MISSING  $sym: write-class, imported by postgres, not interposed by the shim"
        rc=1
      fi
    done < "$tmp/used"
  done
done

if [ "$rc" -ne 0 ]; then
  cat >&2 <<'EOF'

pg-import-audit: FAIL. Interpose the MISSING functions in internal/cow/lazyrw/lazyrw.c
(then make lazyrw), or, if Postgres provably never uses them on a relation
file's fd, add them to internal/cow/lazyrw/audit/allow.txt with the reason.
EOF
  exit 1
fi
echo "pg-import-audit: every write-class import is interposed or allowlisted"
