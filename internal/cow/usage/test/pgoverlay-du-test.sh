#!/bin/sh
# Functional test for pgoverlay-du.
#
#   internal/cow/usage/test/pgoverlay-du-test.sh BINARY DIR
#
# BINARY is a built pgoverlay-du; DIR is a scratch directory the test may fill
# and empty (it creates DIR/pgoverlay-du-test.$$ and removes it). Needs GNU
# coreutils (cp --reflink) and runs as any user. On a filesystem that
# reflinks (XFS reflink=1, btrfs) it checks the shared/exclusive split of
# clones and, on XFS, the copy-on-write extent size hint (-c); elsewhere it
# checks that nothing is reported shared. It exits non-zero on the first
# failed check.
set -eu

BIN=$1
DIR=$2
T=$DIR/pgoverlay-du-test.$$
mkdir "$T"
trap 'rm -rf "$T"' EXIT

MIB=1048576
fails=0
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; fails=$((fails + 1)); }

# field N of the totals line for PATH: 1 exclusive, 2 shared, 3 apparent
f() { "$BIN" -- "$2" | cut -f"$1"; }
# within LO <= V <= HI
within() { [ "$1" -ge "$2" ] && [ "$1" -le "$3" ]; }

fstype=$(stat -f -c %T "$T")
echo "filesystem: $fstype ($T)"

"$BIN" -V | grep -q '^pgoverlay-du ' && pass "version" || fail "version"

# usage errors exit 2
if "$BIN" >/dev/null 2>&1; then fail "no args accepted"; else [ $? -eq 2 ] && pass "no args: exit 2" || fail "no args: wrong exit status"; fi
if "$BIN" -- "$T/missing" >/dev/null 2>&1; then fail "missing path accepted"; else [ $? -eq 2 ] && pass "missing path: exit 2" || fail "missing path: wrong exit status"; fi

# a plain 8 MiB file: all exclusive, apparent = size
mkdir "$T/tree"
dd if=/dev/urandom of="$T/tree/a" bs=$MIB count=8 2>/dev/null
sync
e=$(f 1 "$T/tree/a"); s=$(f 2 "$T/tree/a"); a=$(f 3 "$T/tree/a")
within "$e" $((8 * MIB)) $((9 * MIB)) && [ "$s" -eq 0 ] && [ "$a" -eq $((8 * MIB)) ] &&
	pass "plain file: exclusive=$e shared=$s apparent=$a" || fail "plain file: exclusive=$e shared=$s apparent=$a"

# the output parses like du -sb: the first field is a byte count, the last the path
line=$("$BIN" -- "$T/tree")
[ "$(printf '%s' "$line" | cut -f4)" = "$T/tree" ] && pass "totals line names the path" || fail "totals line: $line"

# a hard link is counted once; a symlink is not followed
ln "$T/tree/a" "$T/tree/a-link"
ln -s "$T/tree/a" "$T/tree/a-sym"
a=$(f 3 "$T/tree")
within "$a" $((8 * MIB)) $((8 * MIB + 65536)) && pass "hard link counted once, symlink not followed (apparent=$a)" || fail "hard link/symlink: apparent=$a"
rm "$T/tree/a-link" "$T/tree/a-sym"

# an unreadable file is an error (exit 1) but the totals are still printed;
# root reads everything, so only check this as another user
if [ "$(id -u)" != 0 ]; then
	dd if=/dev/zero of="$T/tree/locked" bs=4096 count=1 2>/dev/null
	chmod 000 "$T/tree/locked"
	set +e
	out=$("$BIN" -- "$T/tree" 2>/dev/null)
	rc=$?
	set -e
	[ $rc -eq 1 ] && [ -n "$out" ] && pass "unreadable file: exit 1 with totals" || fail "unreadable file: rc=$rc out=$out"
	chmod 600 "$T/tree/locked"
	rm "$T/tree/locked"
fi

if cp --reflink=always "$T/tree/a" "$T/tree/b" 2>/dev/null; then
	sync
	# a clone shares all of its extents; the pair owns nothing alone
	e=$(f 1 "$T/tree/b"); s=$(f 2 "$T/tree/b")
	[ "$e" -eq 0 ] && within "$s" $((8 * MIB)) $((9 * MIB)) && pass "clone: exclusive=$e shared=$s" || fail "clone: exclusive=$e shared=$s"
	e=$(f 1 "$T/tree"); s=$(f 2 "$T/tree"); a=$(f 3 "$T/tree")
	within "$e" 0 65536 && within "$s" $((16 * MIB)) $((18 * MIB)) && within "$a" $((16 * MIB)) $((16 * MIB + 65536)) &&
		pass "tree with original and clone: exclusive=$e shared=$s apparent=$a" || fail "tree: exclusive=$e shared=$s apparent=$a"

	# rewriting one 8 KiB page of the clone unshares at most one CoW extent
	# (XFS default 128 KiB; btrfs one 4 KiB block per page block)
	dd if=/dev/urandom of="$T/tree/b" bs=8192 count=1 seek=100 conv=notrunc 2>/dev/null
	sync
	e=$(f 1 "$T/tree/b"); s=$(f 2 "$T/tree/b")
	within "$e" 8192 $((1 * MIB)) && within "$s" $((7 * MIB)) $((8 * MIB)) &&
		pass "clone after one page write: exclusive=$e shared=$s" || fail "clone after one page write: exclusive=$e shared=$s"

	if [ "$fstype" = xfs ]; then
		# the hint: set, read back, inherited by new files, and it narrows
		# the unshare of a page write to the hint
		mkdir "$T/hint"
		out=$("$BIN" -c 16384 "$T/hint")
		[ "$out" = "$(printf 'cowextsize=16384\t%s' "$T/hint")" ] && pass "-c sets the hint: $out" || fail "-c: $out"
		cp --reflink=always "$T/tree/a" "$T/hint/c"
		sync
		dd if=/dev/urandom of="$T/hint/c" bs=8192 count=1 seek=100 conv=notrunc 2>/dev/null
		sync
		e=$(f 1 "$T/hint/c")
		within "$e" 8192 32768 && pass "page write under a 16 KiB hint unshares $e bytes" || fail "page write under a 16 KiB hint unshares $e bytes"
		out=$("$BIN" -c 0 "$T/hint")
		[ "$out" = "$(printf 'cowextsize=0\t%s' "$T/hint")" ] && pass "-c 0 clears the hint" || fail "-c 0: $out"
		if "$BIN" -c 12x "$T/hint" >/dev/null 2>&1; then fail "-c accepted 12x"; else pass "-c rejects a malformed size"; fi
	fi
else
	echo "skip reflink checks: $fstype does not reflink here"
	cp "$T/tree/a" "$T/tree/b"
	sync
	s=$(f 2 "$T/tree")
	[ "$s" -eq 0 ] && pass "copy: nothing shared" || fail "copy: shared=$s"
	if "$BIN" -c 16384 "$T/tree" >/dev/null 2>&1; then
		echo "note: $fstype accepted a copy-on-write extent size hint"
	else
		pass "-c fails where the hint does not exist"
	fi
fi

if [ $fails -ne 0 ]; then
	echo "$fails check(s) failed"
	exit 1
fi
echo "all checks passed"
