#!/usr/bin/env bash
# Tests for hack/vulncheck.sh (make vuln-test). Each case runs the real gate
# against a stub govulncheck that replays a canned JSON stream and exit code,
# and asserts the gate's exit status and output. Needs only bash and jq: no
# network, no scanner install, no build.
set -euo pipefail
cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The stub stands in for govulncheck: prints $STUB_JSON (a file) and exits
# $STUB_RC. Anything on $STUB_STDERR goes to stderr.
cat >"$work/govulncheck" <<'EOF'
#!/usr/bin/env bash
[ -n "${STUB_STDERR:-}" ] && echo "$STUB_STDERR" >&2
[ -n "${STUB_JSON:-}" ] && cat "$STUB_JSON"
exit "${STUB_RC:-0}"
EOF
chmod +x "$work/govulncheck"
: >"$work/fakebin"

header='{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.8.0","scan_level":"symbol","scan_mode":"binary"}}
{"progress":{"message":"Scanning your binary for known vulnerabilities..."}}'

osv() { # id summary
  printf '{"osv":{"id":"%s","summary":"%s"}}\n' "$1" "$2"
}
hit() { # id module version fixed function
  local fixed=""
  [ -n "$4" ] && fixed=",\"fixed_version\":\"$4\""
  local fn=""
  [ -n "$5" ] && fn=",\"package\":\"$2/p\",\"function\":\"$5\""
  printf '{"finding":{"osv":"%s"%s,"trace":[{"module":"%s","version":"%s"%s}]}}\n' \
    "$1" "$fixed" "$2" "$3" "$fn"
}

allow="$work/allow.txt"
cat >"$allow" <<'EOF'
# test allowlist
GO-2026-4887 github.com/docker/docker 2026-12-31 client only
EOF

# Results go to a file, not shell counters: several cases pipe their stream
# into expect, which then runs in a subshell.
: >"$work/results"
# expect <name> <want-exit> <want-substring> ; stream on stdin
expect() {
  local name=$1 want=$2 needle=$3
  cat >"$work/stream.json"
  local got=0
  STUB_JSON="$work/stream.json" GOVULNCHECK="$work/govulncheck" \
    PGOVERLAY_VULN_BINS="$work/fakebin" PGOVERLAY_VULN_ALLOWLIST="${ALLOW:-$allow}" \
    PGOVERLAY_VULN_TODAY="${TODAY:-2026-09-28}" \
    hack/vulncheck.sh >"$work/out" 2>&1 || got=$?
  if [ "$got" = "$want" ] && grep -qF -- "$needle" "$work/out"; then
    echo "ok   $name" | tee -a "$work/results"
  else
    echo "FAIL $name: exit $got (want $want), output:" >&2
    sed 's/^/     /' "$work/out" >&2
    echo "FAIL $name" >>"$work/results"
  fi
}

# --- the scan itself must have happened (fail closed) ---
STUB_RC=1 STUB_STDERR="fetching vulnerabilities: dial tcp: lookup vuln.go.dev: no such host" \
  expect "scanner error fails closed" 2 "nothing was scanned" </dev/null
expect "empty output fails closed" 2 "not a complete JSON stream" </dev/null
printf 'Scanning your code...\nNo vulnerabilities found.\n' |
  expect "text output fails closed" 2 "not a complete JSON stream"
echo '{"progress":{"message":"x"}}' |
  expect "stream without config fails closed" 2 "not a complete JSON stream"
echo "$header" | head -1 |
  expect "stream without progress fails closed" 2 "not a complete JSON stream"
expect "clean stream passes" 0 "OK" <<<"$header"

# --- the decision ---
{ echo "$header"; osv GO-2026-6089 "net/http h2c"; hit GO-2026-6089 stdlib v1.26.5 v1.26.6 Serve; } |
  expect "reachable stdlib advisory fails" 1 "GO-2026-6089 in stdlib@v1.26.5 is reachable"
{ echo "$header"; osv GO-2026-9999 "new moby advisory"; hit GO-2026-9999 github.com/docker/docker v28.5.2 "" Do; } |
  expect "new advisory on the allowlisted module is not accepted by module" 1 "GO-2026-9999"
{ echo "$header"; osv GO-2026-4887 "authz"; hit GO-2026-4887 github.com/docker/docker v28.5.2 "" Do; } |
  expect "allowlisted advisory passes" 0 "allowed: GO-2026-4887"
{ echo "$header"; osv GO-2026-4887 "authz"; hit GO-2026-4887 github.com/docker/docker v28.5.2 "" Do; } |
  TODAY=2027-01-01 expect "expired entry fails" 1 "expired on 2026-12-31"
{ echo "$header"; osv GO-2026-4887 "authz"; hit GO-2026-4887 github.com/docker/docker v28.5.2 v28.5.3 Do; } |
  expect "entry whose advisory gained a fix fails" 1 "now has a fix in github.com/docker/docker v28.5.3"
{ echo "$header"; osv GO-2026-4887 "authz"; hit GO-2026-4887 github.com/moby/moby/v2 v2.0.0 "" Do; } |
  expect "entry for a different module fails" 1 "reaches it through github.com/moby/moby/v2"
{ echo "$header"; osv GO-2026-5746 "archive"; hit GO-2026-5746 github.com/docker/docker v28.5.2 "" ""; } |
  expect "module-level finding does not gate" 0 "1 more advisories"
expect "unmatched entry warns" 0 "GO-2026-4887 is allowlisted but no scanned binary reaches it" <<<"$header"

# --- the allowlist must parse ---
printf 'GO-2026-4887 github.com/docker/docker someday reason\n' >"$work/bad.txt"
ALLOW="$work/bad.txt" expect "malformed expiry is rejected" 2 "no YYYY-MM-DD expiry" <<<"$header"
printf 'GO-2026-4887 github.com/docker/docker 2026-12-31\n' >"$work/bad.txt"
ALLOW="$work/bad.txt" expect "entry without a reason is rejected" 2 "has no reason" <<<"$header"

pass=$(grep -c '^ok' "$work/results" || true)
fail=$(grep -c '^FAIL' "$work/results" || true)
echo "vulncheck_test: $pass passed, $fail failed"
[ "$fail" -eq 0 ] && [ "$pass" -gt 0 ]
