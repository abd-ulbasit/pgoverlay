#!/bin/sh
# Runs the lazyrw C tests on a real OverlayFS mount. Needs root with
# CAP_SYS_ADMIN (a privileged container: hack/build-lazyrw.sh test).
#
#   run.sh <liblazyrw.so> <lazyrw_test binary>
#
# Two layouts, because OverlayFS reports file identity differently in each:
#   split: lower and upper on separate tmpfs mounts, upper limited to 24 MiB so
#          that a whole-file copy-up of a 32 MiB lower file cannot fit;
#   same:  lower and upper on one tmpfs (what Docker volumes on one disk give).
set -eu
so=$1
bin=$2
[ -f "$so" ] && [ -x "$bin" ] || { echo "usage: $0 <liblazyrw.so> <lazyrw_test>" >&2; exit 2; }
case $so in /*) ;; *) so=$PWD/$so ;; esac

W=$(mktemp -d /tmp/lazyrw-test.XXXXXX)
failed=0
cleanup() {
  cd /
  umount "$W/merged" 2>/dev/null || true
  umount "$W/upperfs" 2>/dev/null || true
  umount "$W/lowerfs" 2>/dev/null || true
  rm -rf "$W"
}
trap cleanup EXIT INT TERM

mkdir -p "$W/bin" "$W/lowerfs" "$W/upperfs" "$W/merged"
cp "$bin" "$W/bin/postgres"
cp "$bin" "$W/bin/notpostgres"

# mount_overlay <layout>: a fresh upper over the fixture lower layer.
mount_overlay() {
  cd /
  umount "$W/merged" 2>/dev/null || true
  umount "$W/upperfs" 2>/dev/null || true
  case $1 in
    split) mount -t tmpfs -o size=24m tmpfs "$W/upperfs"; up=$W/upperfs ;;
    same)  rm -rf "$W/lowerfs/rw"; up=$W/lowerfs/rw ;;
  esac
  mkdir -p "$up/upper" "$up/work"
  mount -t overlay overlay \
    -o "lowerdir=$W/lowerfs/data,upperdir=$up/upper,workdir=$up/work" "$W/merged"
  UPPER=$up/upper
  cd "$W/merged"
}

# run <name> <expect-stderr: empty|any> <cmd...>: one suite, output checked.
run() {
  name=$1 expect=$2
  shift 2
  echo "# $name"
  if "$@" 2>"$W/stderr"; then rc=0; else rc=$?; fi
  if [ "$rc" -ne 0 ]; then
    echo "not ok - $name: $rc failed check(s)"
    failed=$((failed + 1))
  fi
  if [ "$expect" = empty ] && [ -s "$W/stderr" ]; then
    echo "not ok - $name: unexpected stderr:"
    sed 's/^/#   /' "$W/stderr"
    failed=$((failed + 1))
  fi
}

mount -t tmpfs -o size=256m tmpfs "$W/lowerfs"
"$W/bin/postgres" mkfiles "$W/lowerfs/data"

echo "# kernel $(uname -r) $(uname -m); shim $(basename "$so")"

# A non-postgres process with the shim preloaded prints nothing: the branch
# entrypoint's preload probe relies on that.
if ! LD_PRELOAD=$so /bin/sh -c 'true' 2>"$W/stderr" || [ -s "$W/stderr" ]; then
  echo "not ok - preload probe: a non-postgres process was not silent:"
  sed 's/^/#   /' "$W/stderr"
  failed=$((failed + 1))
else
  echo "ok - preload probe: silent in a non-postgres process"
fi

for layout in split same; do
  mount_overlay "$layout"
  run "active ($layout layout)" empty \
    env PGDATA="$W/merged" LD_PRELOAD="$so" "$W/bin/postgres" active "$UPPER"
done

mount_overlay split
run "enospc" any env PGDATA="$W/merged" LD_PRELOAD="$so" "$W/bin/postgres" enospc "$UPPER"
if grep -q 'pgoverlay-lazyrw: .*could not upgrade fd .*No space left on device' "$W/stderr"; then
  echo "ok - enospc: the failed upgrade was logged to stderr"
else
  echo "not ok - enospc: no upgrade-failure line on stderr:"
  sed 's/^/#   /' "$W/stderr"
  failed=$((failed + 1))
fi
if [ "$(grep -c 'pgoverlay-lazyrw:' "$W/stderr")" -eq 1 ]; then
  echo "ok - enospc: repeated failures within a second are rate-limited to one line"
else
  echo "not ok - enospc: expected exactly one log line, got $(grep -c 'pgoverlay-lazyrw:' "$W/stderr")"
  failed=$((failed + 1))
fi

mount_overlay split
run "noshim (control)" empty env PGDATA="$W/merged" "$W/bin/postgres" noshim "$UPPER"

mount_overlay split
run "inert: not postgres" empty env PGDATA="$W/merged" LD_PRELOAD="$so" "$W/bin/notpostgres" inert "$UPPER"
run "inert: no PGDATA" empty env -u PGDATA LD_PRELOAD="$so" "$W/bin/postgres" nopgdata "$UPPER"
run "maxfd" empty env PGDATA="$W/merged" LD_PRELOAD="$so" "$W/bin/postgres" maxfd "$UPPER"

run "debug" any env PGDATA="$W/merged" LD_PRELOAD="$so" PGOVERLAY_LAZYRW_DEBUG=1 "$W/bin/postgres" debug "$UPPER"
if grep -q 'downgrade->RDONLY .*base/1/debug' "$W/stderr" && grep -q 'COPY-UP on first write .*base/1/debug' "$W/stderr"; then
  echo "ok - debug: PGOVERLAY_LAZYRW_DEBUG logs the downgrade and the upgrade"
else
  echo "not ok - debug: missing debug lines:"
  sed 's/^/#   /' "$W/stderr"
  failed=$((failed + 1))
fi

if [ "$failed" -ne 0 ]; then
  echo "FAIL: $failed suite(s) failed ($(basename "$so"))"
  exit 1
fi
echo "PASS ($(basename "$so"))"
