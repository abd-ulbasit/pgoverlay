#!/usr/bin/env bash
# govulncheck gate: binary mode + a MODULE-scoped allowlist. Run by `make vuln`
# and by the `vuln` CI job, so the gate is identical in both places.
#
# Why binary mode: it scans the deps actually compiled into the shipped
# binaries and avoids source-version skew.
#
# Why the allowlist is a module and not a list of IDs: Moby ships
# plugin-privilege and `docker cp` advisories faster than it ships fixed
# releases for github.com/docker/docker (four so far, none with a fix on that
# module path), so an ID list is a standing false alarm that trains you to
# ignore the one job whose purpose is to be believed. pgbranch drives the
# Docker client only to manage branch containers: it installs no plugins and
# never calls `docker cp`, so those paths are not reachable. Rationale and the
# current advisory list live in SECURITY.md.
#
# Anything outside github.com/docker/docker still fails, including any future
# stdlib CVE the pinned toolchain has not patched.
#
# The allowlist is not a promise anyone has to remember: the "allowlist has
# come due" check below fails the build the day an allowlisted advisory gains
# a fixed version ON THE ALLOWLISTED MODULE PATH — which is exactly the
# condition SECURITY.md says will retire the allowlist.
set -euo pipefail
cd "$(dirname "$0")/.."

ALLOW_MODULE=${PGBRANCH_VULN_ALLOW_MODULE:-github.com/docker/docker}

command -v jq >/dev/null || { echo "vuln: jq is required (brew install jq / apt install jq)" >&2; exit 2; }

GOVC=$(command -v govulncheck || true)
if [ -z "$GOVC" ]; then
  echo "vuln: installing golang.org/x/vuln/cmd/govulncheck@latest"
  go install golang.org/x/vuln/cmd/govulncheck@latest
  GOVC="$(go env GOPATH)/bin/govulncheck"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

go build -o "$work/branchd" ./cmd/branchd
go build -o "$work/pgbranch-github" ./cmd/pgbranch-github

rc=0
for bin in "$work/branchd" "$work/pgbranch-github"; do
  echo "== $(basename "$bin") =="
  # Text mode is the source of truth for WHICH advisories count: in binary mode
  # the JSON lists every advisory affecting every linked module (23 at last
  # count), while text mode reports the subset govulncheck attributes to this
  # binary. JSON is used only to map each of those IDs back to its module and
  # to read the OSV fixed-version events.
  "$GOVC" -mode=binary "$bin" 2>&1 | tee "$work/gv.txt" || true
  "$GOVC" -mode=binary -format=json "$bin" 2>/dev/null > "$work/gv.json" || true

  for id in $(grep -oE 'GO-[0-9]{4}-[0-9]+' "$work/gv.txt" | sort -u); do
    # `|| true`: pipefail would otherwise abort the run on an empty/partial
    # jq stream rather than reporting the advisory as unknown-module.
    mod=$(jq -r --arg i "$id" \
      'select(.finding.osv == $i) | [.finding.trace[]?.module] | join(",")' \
      "$work/gv.json" 2>/dev/null | sort -u | head -1 || true)
    if [ "$mod" != "$ALLOW_MODULE" ]; then
      echo "::error::$id in ${mod:-unknown} is not allowlisted (only $ALLOW_MODULE is)"
      rc=1
      continue
    fi
    # Allowlisted. Has upstream shipped a fix on this module path yet?
    fixed=$(jq -r --arg i "$id" --arg m "$ALLOW_MODULE" \
      'select(.osv.id == $i) | .osv.affected[]? | select(.package.name == $m)
       | [.ranges[]?.events[]?.fixed] | map(select(. != null)) | join(",")' \
      "$work/gv.json" 2>/dev/null | sort -u | tr -d '[:space:]' || true)
    if [ -n "$fixed" ]; then
      echo "::error::$id now has a fix in $ALLOW_MODULE $fixed — bump the dependency and drop it from the SECURITY.md allowlist"
      rc=1
    fi
  done
done

if [ "$rc" -eq 0 ]; then
  echo "only unreachable $ALLOW_MODULE advisories present, none with a fix on that module path — OK"
fi
exit $rc
