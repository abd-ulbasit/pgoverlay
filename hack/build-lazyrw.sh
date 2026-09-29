#!/usr/bin/env bash
# Builds and tests the lazyrw LD_PRELOAD shim (internal/cow/lazyrw) for glibc
# and musl, and the static pgoverlay-du usage tool (internal/cow/usage), on
# linux/amd64 and linux/arm64.
#
#   hack/build-lazyrw.sh [build]   rebuild internal/cow/lazyrw/dist and
#                                  internal/cow/usage/dist, each with its
#                                  SHA256SUMS
#   hack/build-lazyrw.sh check     rebuild into a temporary directory and fail
#                                  unless both dist/ directories are
#                                  byte-identical to it (CI)
#   hack/build-lazyrw.sh test [PLATFORM...]
#                                  run the C tests (internal/cow/lazyrw/test) on
#                                  a real OverlayFS mount, against the committed
#                                  dist/ builds, in privileged containers, then
#                                  preload each build into real postgres images,
#                                  then run the pgoverlay-du functional test
#                                  (internal/cow/usage/test) on the committed
#                                  binary; the default platform is the Docker
#                                  host's own
#
# The builds are reproducible (see internal/cow/lazyrw/Dockerfile), and the
# results are committed so that `go install` needs no C toolchain.
#
# Needs Docker with buildx. Unless BUILDX_BUILDER names one, the script uses
# (and on first use creates) a docker-container builder named
# pgoverlay-lazyrw; its bundled QEMU builds the foreign platform with no
# binfmt_misc set-up. Remove it with `docker buildx rm pgoverlay-lazyrw`.
# `test` on a foreign platform runs that platform's images, so it does need
# binfmt_misc (CI installs it with docker/setup-qemu-action).
#
# Environment:
#   LAZYRW_PLATFORMS   build platforms (default linux/amd64,linux/arm64)
#   LAZYRW_NO_CACHE    1: build without the builder's cache
#   LAZYRW_TEST_NAME   container name prefix for `test` (default pgoverlay-lazyrw-test)
#   LAZYRW_PROBE_IMAGES  postgres images `test` preloads each build into
#                      (default postgres:18 postgres:17-bookworm postgres:17-alpine)
#   BUILDX_BUILDER     buildx builder to use instead of pgoverlay-lazyrw
set -euo pipefail
cd "$(dirname "$0")/.."

DIR=internal/cow/lazyrw
DIST=$DIR/dist
# pgoverlay-du has its own directory (package cow embeds usage/dist); its
# README.md is not a build output.
DU_DIR=internal/cow/usage
DU_DIST=$DU_DIR/dist
DOCKERFILE=$DIR/Dockerfile
CONTEXT=internal/cow
PLATFORMS=${LAZYRW_PLATFORMS:-linux/amd64,linux/arm64}
TEST_NAME=${LAZYRW_TEST_NAME:-pgoverlay-lazyrw-test}
PROBE_IMAGES=${LAZYRW_PROBE_IMAGES:-postgres:18 postgres:17-bookworm postgres:17-alpine}

die() { echo "build-lazyrw: $*" >&2; exit 1; }

docker buildx version >/dev/null 2>&1 || die "docker buildx is required (https://docs.docker.com/go/buildx/)"

if [ -z "${BUILDX_BUILDER:-}" ]; then
  export BUILDX_BUILDER=pgoverlay-lazyrw
  if ! docker buildx inspect "$BUILDX_BUILDER" >/dev/null 2>&1; then
    docker buildx create --name "$BUILDX_BUILDER" --driver docker-container >/dev/null
  fi
fi

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/build-lazyrw.XXXXXX")
trap 'rm -rf "$tmp"' EXIT

# buildx_out <target> <platforms> <dest>: the target's files, flattened into dest.
buildx_out() {
  local target=$1 platforms=$2 dest=$3
  local nocache=()
  [ "${LAZYRW_NO_CACHE:-}" = 1 ] && nocache=(--no-cache)
  docker buildx build ${nocache[@]+"${nocache[@]}"} --target "$target" --platform "$platforms" \
    -f "$DOCKERFILE" --output "type=local,dest=$tmp/raw-$target" "$CONTEXT" >/dev/null
  mkdir -p "$dest"
  # One platform writes the files at the top; several write one directory each.
  find "$tmp/raw-$target" -type f -exec cp {} "$dest/" \;
  rm -rf "$tmp/raw-$target"
}

# sums <dir>: dir/SHA256SUMS for every file in dir.
sums() {
  (cd "$1" && find . -maxdepth 1 -type f | sed 's|^\./||' | LC_ALL=C sort |
    while read -r f; do sha256 "$f"; done > "$tmp/SHA256SUMS")
  mv "$tmp/SHA256SUMS" "$1/SHA256SUMS"
}

# build_dist <dest>: every shim variant in dest/lazyrw and every pgoverlay-du
# binary in dest/usage, each directory with its own SHA256SUMS.
build_dist() {
  local dest=$1
  buildx_out dist "$PLATFORMS" "$dest/all"
  mkdir -p "$dest/lazyrw" "$dest/usage"
  mv "$dest/all"/*.so "$dest/lazyrw/"
  mv "$dest/all"/pgoverlay-du-* "$dest/usage/"
  rmdir "$dest/all" || die "unexpected build outputs: $(ls "$dest/all")"
  chmod 0644 "$dest/lazyrw"/*.so
  chmod 0755 "$dest/usage"/pgoverlay-du-*
  sums "$dest/lazyrw"
  sums "$dest/usage"
}

cmd=${1:-build}
case $cmd in
  build)
    build_dist "$tmp/dist"
    rm -f "$DIST"/*
    mkdir -p "$DIST"
    cp -p "$tmp/dist/lazyrw"/* "$DIST/"
    rm -f "$DU_DIST"/pgoverlay-du-* "$DU_DIST/SHA256SUMS"
    mkdir -p "$DU_DIST"
    cp -p "$tmp/dist/usage"/* "$DU_DIST/"
    (cd "$DIST" && cat SHA256SUMS)
    (cd "$DU_DIST" && cat SHA256SUMS)
    ;;

  check)
    build_dist "$tmp/dist"
    rc=0
    for pair in "lazyrw:$DIST" "usage:$DU_DIST"; do
      fresh=$tmp/dist/${pair%%:*}
      committed=${pair#*:}
      if ! (cd "$committed" && sha256 -c --quiet SHA256SUMS); then
        echo "build-lazyrw: $committed does not match its SHA256SUMS" >&2
        rc=1
      elif ! diff -r -x README.md "$fresh" "$committed" >"$tmp/diff" 2>&1; then
        cat "$tmp/diff" >&2
        echo "--- fresh build:" >&2
        cat "$fresh/SHA256SUMS" >&2
        echo "build-lazyrw: $committed is not what its sources build: run 'make lazyrw' and commit the result" >&2
        rc=1
      else
        echo "ok: $committed is reproducible from source"
        (cd "$committed" && cat SHA256SUMS)
      fi
    done
    exit $rc
    ;;

  test)
    shift
    platforms=("$@")
    if [ ${#platforms[@]} -eq 0 ]; then
      case $(docker info --format '{{.Architecture}}') in
        x86_64 | amd64) platforms=(linux/amd64) ;;
        aarch64 | arm64) platforms=(linux/arm64) ;;
        *) die "unsupported Docker host architecture" ;;
      esac
    fi
    debian=$(sed -n 's/^ARG DEBIAN_IMAGE=//p' "$DOCKERFILE")
    alpine=$(sed -n 's/^ARG ALPINE_IMAGE=//p' "$DOCKERFILE")
    rc=0
    for p in "${platforms[@]}"; do
      case $p in
        linux/amd64) arch=x86_64 ;;
        linux/arm64) arch=aarch64 ;;
        *) die "unsupported platform $p" ;;
      esac
      buildx_out tests "$p" "$tmp/tests"
      for libc in glibc musl; do
        so=liblazyrw-$libc-$arch.so
        bin=lazyrw_test-$libc-$arch
        [ -f "$DIST/$so" ] || die "$DIST/$so is missing: run 'make lazyrw'"
        image=$debian
        [ "$libc" = musl ] && image=$alpine
        stage=$tmp/stage-$libc-$arch
        mkdir -p "$stage"
        cp "$DIR/test/run.sh" "$DIST/$so" "$tmp/tests/$bin" "$stage/"
        echo "=== $so on $p ($image)"
        # The files go in on stdin, so this works with a remote Docker engine.
        if ! tar -cf - -C "$stage" . | docker run --rm -i --privileged --platform "$p" \
          --name "$TEST_NAME-$libc-$arch" "$image" \
          sh -c 'mkdir -p /w && tar -xf - -C /w && sh /w/run.sh "/w/$1" "/w/$2"' sh "$so" "$bin"; then
          rc=1
        fi
      done
      # The builds load, stay silent and let the server run in real postgres
      # images, whose glibc (2.36, 2.41) and musl are newer than the build's.
      for image in $PROBE_IMAGES; do
        libc=glibc
        case $image in *alpine*) libc=musl ;; esac
        so=liblazyrw-$libc-$arch.so
        out=$(tar -cf - -C "$DIST" "$so" | docker run --rm -i --quiet --platform "$p" --entrypoint sh \
          --name "$TEST_NAME-probe-$arch" "$image" \
          -c 'mkdir -p /w && tar -xf - -C /w && LD_PRELOAD="/w/$1" /bin/true && LD_PRELOAD="/w/$1" postgres --version' \
          sh "$so" 2>&1) || true
        if printf '%s\n' "$out" | grep -Eqx 'postgres \(PostgreSQL\) [0-9.]+.*' && [ "$(printf '%s\n' "$out" | wc -l)" -eq 1 ]; then
          echo "ok - $so preloads silently in $image ($p): $out"
        else
          echo "not ok - $so in $image ($p):"
          printf '%s\n' "$out" | sed 's/^/#   /'
          rc=1
        fi
      done
      # pgoverlay-du is static, so any image runs it. The functional test uses
      # the container's own filesystem and runs as nobody, so that its
      # unreadable-file case runs too; reflink cases run only where the
      # filesystem clones.
      du=pgoverlay-du-$arch
      [ -f "$DU_DIST/$du" ] || die "$DU_DIST/$du is missing: run 'make lazyrw'"
      stage=$tmp/stage-du-$arch
      mkdir -p "$stage"
      cp "$DU_DIR/test/pgoverlay-du-test.sh" "$DU_DIST/$du" "$stage/"
      echo "=== $du on $p ($debian)"
      if ! tar -cf - -C "$stage" . | docker run --rm -i --platform "$p" --user 65534:65534 \
        --name "$TEST_NAME-du-$arch" "$debian" \
        sh -c 'mkdir -p /tmp/w && tar -xf - -C /tmp/w && sh /tmp/w/pgoverlay-du-test.sh "/tmp/w/$1" /tmp/w' sh "$du"; then
        rc=1
      fi
    done
    exit $rc
    ;;

  *)
    die "unknown command $cmd (want build, check or test)"
    ;;
esac
