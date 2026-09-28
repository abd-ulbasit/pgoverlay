#!/usr/bin/env bash
# govulncheck gate: binary mode + an explicit, expiring allowlist of advisory
# IDs. Run by `make vuln` and by the `vuln` CI job, so the gate is identical in
# both places.
#
# Why binary mode: it scans what is actually compiled into the shipped
# binaries (pgb, branchd, pgoverlay-github), built for linux/amd64 like the
# images, and avoids source-version skew.
#
# What fails the gate:
#   - any symbol-level finding (the set govulncheck's text output reports as
#     "Your code is affected by ...") that is not listed in
#     hack/vuln-allowlist.txt, stdlib included;
#   - an allowlisted finding in a different module than its entry names;
#   - an allowlisted finding whose entry has expired;
#   - an allowlisted finding that now has a fixed version on its module, so
#     the fix is one `go get` away;
#   - govulncheck itself failing, or printing anything other than a complete
#     govulncheck JSON stream. The gate fails closed: a scan that did not run
#     is never reported as a clean scan.
# Module- and package-level findings (vulnerable code linked in but never
# called) are printed as a count and do not fail the build.
#
# Exit status: 0 pass, 1 findings, 2 the scan or the allowlist is broken.
#
# Knobs, mostly for hack/vulncheck_test.sh:
#   GOVULNCHECK_VERSION        scanner version to install (pinned below)
#   GOVULNCHECK                use this govulncheck binary instead of installing
#   PGOVERLAY_VULN_ALLOWLIST   allowlist file (default hack/vuln-allowlist.txt)
#   PGOVERLAY_VULN_BINS        space-separated binaries to scan instead of building
#   PGOVERLAY_VULN_TODAY       YYYY-MM-DD to evaluate expiry against (default: today, UTC)
set -euo pipefail
cd "$(dirname "$0")/.."

GOVULNCHECK_VERSION=${GOVULNCHECK_VERSION:-v1.8.0}
ALLOWLIST=${PGOVERLAY_VULN_ALLOWLIST:-hack/vuln-allowlist.txt}
TODAY=${PGOVERLAY_VULN_TODAY:-$(date -u +%Y-%m-%d)}

die() { echo "::error::vuln: $*" >&2; exit 2; }

command -v jq >/dev/null || die "jq is required (brew install jq / apt install jq)"
[ -r "$ALLOWLIST" ] || die "allowlist $ALLOWLIST is missing"

# Validate the allowlist up front: a typo must not silently accept or reject.
while read -r id mod exp reason; do
  case "$id" in '' | '#'*) continue ;; esac
  [[ "$id" =~ ^GO-[0-9]{4}-[0-9]+$ ]] || die "$ALLOWLIST: bad advisory ID '$id'"
  [ -n "$mod" ] || die "$ALLOWLIST: $id has no module"
  [[ "$exp" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || die "$ALLOWLIST: $id has no YYYY-MM-DD expiry"
  [ -n "$reason" ] || die "$ALLOWLIST: $id has no reason"
done <"$ALLOWLIST"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Install the pinned scanner into the scratch dir. The path is then known no
# matter how GOBIN/GOPATH are set, and a new govulncheck release cannot change
# the gate's behaviour without a commit here.
GOVC=${GOVULNCHECK:-}
if [ -z "$GOVC" ]; then
  echo "vuln: installing golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION"
  GOBIN="$work/bin" go install "golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION" ||
    die "could not install govulncheck@$GOVULNCHECK_VERSION"
  GOVC="$work/bin/govulncheck"
fi
[ -x "$GOVC" ] || die "govulncheck binary $GOVC is not executable"

if [ -n "${PGOVERLAY_VULN_BINS:-}" ]; then
  read -r -a bins <<<"$PGOVERLAY_VULN_BINS"
else
  bins=()
  for cmd in pgb branchd pgoverlay-github; do
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$work/$cmd" "./cmd/$cmd"
    bins+=("$work/$cmd")
  done
fi

rc=0
seen=" "
for bin in "${bins[@]}"; do
  name=$(basename "$bin")
  out="$work/$name.json"
  echo "== $name =="
  if ! "$GOVC" -mode=binary -format=json "$bin" >"$out" 2>"$work/$name.err"; then
    cat "$work/$name.err" >&2
    die "govulncheck failed on $name; nothing was scanned"
  fi
  # JSON mode exits 0 whether or not it finds anything, so the exit status
  # alone does not prove a scan happened. Require the stream's header (config)
  # and the scan's progress messages; anything else is an unreadable result.
  jq -e -s '
    (map(select(.config.scanner_name == "govulncheck"
                and (.config.protocol_version | startswith("v1."))))
       | length == 1)
    and (map(select(.progress)) | length > 0)' "$out" >/dev/null 2>&1 ||
    die "govulncheck output for $name is not a complete JSON stream; refusing to treat it as clean"

  # Symbol-level findings: the vulnerable function is in the binary.
  jq -r -s '
    [.[] | select(.finding) | .finding | select(.trace[0].function != null)
     | [.osv, .trace[0].module, (.trace[0].version // ""), (.fixed_version // "")]]
    | unique_by(.[0]) | .[] | @tsv' "$out" >"$work/$name.hits"
  quiet=$(jq -s '[.[] | select(.finding) | .finding | select(.trace[0].function == null) | .osv]
                 - [.[] | select(.finding) | .finding | select(.trace[0].function != null) | .osv]
                 | unique | length' "$out")

  while IFS=$'\t' read -r id mod ver fixed; do
    seen="$seen$id "
    summary=$(jq -r -s --arg i "$id" 'first(.[] | select(.osv.id == $i) | .osv.summary) // ""' "$out")
    entry=$(awk -v i="$id" '$1 == i { print; exit }' "$ALLOWLIST")
    if [ -z "$entry" ]; then
      echo "::error::$id in $mod@$ver is reachable from $name: $summary${fixed:+ (fixed in $fixed)}"
      rc=1
      continue
    fi
    read -r _ amod aexp _ <<<"$entry"
    if [ "$amod" != "$mod" ]; then
      echo "::error::$id is allowlisted for $amod, but $name reaches it through $mod"
      rc=1
    elif [[ "$TODAY" > "$aexp" ]]; then
      echo "::error::$id's allowlist entry expired on $aexp; re-review it in $ALLOWLIST and SECURITY.md, or remove the dependency"
      rc=1
    elif [ -n "$fixed" ]; then
      echo "::error::$id now has a fix in $mod $fixed; bump the dependency and drop it from $ALLOWLIST and SECURITY.md"
      rc=1
    else
      echo "allowed: $id in $mod@$ver (until $aexp): $summary"
    fi
  done <"$work/$name.hits"
  echo "($quiet more advisories affect modules or packages $name links but never calls; not gating)"
done

while read -r id _; do
  case "$id" in '' | '#'*) continue ;; esac
  case "$seen" in *" $id "*) ;; *)
    echo "::warning::$id is allowlisted but no scanned binary reaches it any more; remove it from $ALLOWLIST and SECURITY.md"
    ;;
  esac
done <"$ALLOWLIST"

if [ "$rc" -eq 0 ]; then
  echo "vuln: no reachable advisories outside $ALLOWLIST: OK"
fi
exit $rc
