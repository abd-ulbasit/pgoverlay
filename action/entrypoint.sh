#!/usr/bin/env bash
# Create a pgoverlay branch and wait until it is ready. Used by action.yml,
# tested by internal/actiontest against a stub server.
#
# In:  PGOVERLAY_SERVER (required), PGOVERLAY_TOKEN (required),
#      PGOVERLAY_SOURCE (default main), PGOVERLAY_BRANCH (default generated),
#      PGOVERLAY_TTL (seconds, default 3600; 0 = no TTL),
#      PGOVERLAY_PROXY_HOST (host[:port] of the pgoverlay router; default the
#      server's host, port 6432),
#      PGOVERLAY_POLL_MAX / PGOVERLAY_POLL_INTERVAL (default 60 x 5s).
# Out (appended to $GITHUB_OUTPUT):
#      branch — as soon as the server has created the branch, before the
#        ready-wait, so a paired destroy step can clean up a branch that
#        never became ready;
#      once ready: proxy_host, proxy_port, proxy_database, user, and the
#        direct host, port, database; password only when the server rotates
#        per-branch credentials, masked in the log first.
#      The token is never echoed.
set -euo pipefail

: "${PGOVERLAY_SERVER:?PGOVERLAY_SERVER (input server) is required}"
: "${PGOVERLAY_TOKEN:?PGOVERLAY_TOKEN (input token) is required}"

server="${PGOVERLAY_SERVER%/}"
source="${PGOVERLAY_SOURCE:-main}"
ttl="${PGOVERLAY_TTL:-3600}"
name="${PGOVERLAY_BRANCH:-}"
poll_max="${PGOVERLAY_POLL_MAX:-60}"
poll_interval="${PGOVERLAY_POLL_INTERVAL:-5}"

name_re='^[a-z0-9][a-z0-9-]{0,40}$'

case "$ttl" in
  ''|*[!0-9]*) echo "ttl must be a non-negative integer (seconds), got '$ttl'" >&2; exit 1 ;;
esac

if [ -z "$name" ]; then
  rand="$(od -An -tx1 -N3 /dev/urandom | tr -d ' \n')"
  name="t-gha-${GITHUB_RUN_ID:-0}-${rand}"
elif ! [[ "$name" =~ $name_re ]]; then
  echo "invalid branch name '$name': must match $name_re" >&2
  exit 1
fi

# The router endpoint: the proxy_host input, else the server's own host (no
# scheme, credentials, port, path or IPv6 brackets) on port 6432.
authority="${server#*://}"
authority="${authority%%/*}"
authority="${authority%%\?*}"
authority="${authority##*@}"
case "$authority" in
  \[*) server_host="${authority#\[}"; server_host="${server_host%%]*}" ;;
  *) server_host="${authority%%:*}" ;;
esac
proxy_host="$server_host"
proxy_port=6432
if [ -n "${PGOVERLAY_PROXY_HOST:-}" ]; then
  bracketed_re='^\[([0-9A-Fa-f:.]+)\](:([0-9]{1,5}))?$'
  hostport_re='^([A-Za-z0-9._-]+)(:([0-9]{1,5}))?$'
  bare_v6_re='^[0-9A-Fa-f]*:[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*$'
  if [[ "$PGOVERLAY_PROXY_HOST" =~ $bracketed_re ]] || [[ "$PGOVERLAY_PROXY_HOST" =~ $hostport_re ]]; then
    proxy_host="${BASH_REMATCH[1]}"
    proxy_port="${BASH_REMATCH[3]:-6432}"
  elif [[ "$PGOVERLAY_PROXY_HOST" =~ $bare_v6_re ]]; then
    proxy_host="$PGOVERLAY_PROXY_HOST"
  else
    proxy_port=0
  fi
  if [ "$proxy_port" -lt 1 ] || [ "$proxy_port" -gt 65535 ]; then
    echo "invalid proxy_host '$PGOVERLAY_PROXY_HOST': want host[:port] (IPv6 as [addr]:port)" >&2
    exit 1
  fi
fi

body="$(jq -n --arg name "$name" --arg source "$source" --argjson ttl "$ttl" \
  '{name: $name, source: $source, ttl_seconds: $ttl}')"

resp="$(curl -sS -w '\n%{http_code}' \
  -H "Authorization: Bearer $PGOVERLAY_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "$body" "$server/v1/branches")"
code="${resp##*$'\n'}"
payload="${resp%$'\n'*}"
if [ "$code" != "201" ]; then
  # no branch output: nothing was created (a 409 may even name someone
  # else's branch), so the paired destroy step must have nothing to delete
  echo "create branch '$name' failed: HTTP $code: $payload" >&2
  exit 1
fi

# The branch exists from here on: hand its name to the destroy step now, so
# a failed or timed-out wait below still gets cleaned up.
echo "branch=$name" >>"$GITHUB_OUTPUT"

# last_reason prints ": <reason>" of the branch's most recent recorded
# transition, or nothing (best-effort; only enriches the error message).
last_reason() {
  local hist
  hist="$(curl -sS -H "Authorization: Bearer $PGOVERLAY_TOKEN" \
    "$server/v1/branches/$name/history" 2>/dev/null || true)"
  jq -r 'if type == "array" then
      (map(select((.reason // "") != "")) | last | .reason // empty | ": " + .)
    else empty end' <<<"$hist" 2>/dev/null || true
}

# the create endpoint is synchronous today, but poll GET regardless, and
# stop at once on a state the branch can never leave for ready
state="$(jq -r '.state // empty' <<<"$payload")"
tries=0
while [ "$state" != "ready" ]; do
  case "$state" in
    failed|destroying|destroyed)
      echo "branch '$name' is $state and will never become ready$(last_reason)" >&2
      exit 1
      ;;
  esac
  tries=$((tries + 1))
  if [ "$tries" -gt "$poll_max" ]; then
    echo "branch '$name' not ready after $((poll_max * poll_interval))s (state '$state')" >&2
    exit 1
  fi
  sleep "$poll_interval"
  resp="$(curl -sS -w '\n%{http_code}' \
    -H "Authorization: Bearer $PGOVERLAY_TOKEN" "$server/v1/branches/$name")"
  code="${resp##*$'\n'}"
  payload="${resp%$'\n'*}"
  if [ "$code" != "200" ]; then
    echo "poll branch '$name' failed: HTTP $code: $payload" >&2
    exit 1
  fi
  state="$(jq -r '.state // empty' <<<"$payload")"
done

field() { jq -r --arg d "${2:-}" ".$1 // \$d" <<<"$payload"; }
host="$(field host)"
port="$(field port)"
database="$(field database postgres)"
user="$(field user postgres)"
proxy_database="$(field proxy_database "$database@$name")"
# only set when branchd runs --rotate-branch-credentials; in inherit mode the
# workflow already holds the source's password as its own secret
password="$(field password)"
if [ -n "$password" ]; then
  echo "::add-mask::$password"
fi

{
  echo "proxy_host=$proxy_host"
  echo "proxy_port=$proxy_port"
  echo "proxy_database=$proxy_database"
  echo "user=$user"
  echo "host=$host"
  echo "port=$port"
  echo "database=$database"
} >>"$GITHUB_OUTPUT"
if [ -n "$password" ]; then
  echo "password=$password" >>"$GITHUB_OUTPUT"
fi

echo "pgoverlay: branch '$name' ready; connect through the router at $proxy_host:$proxy_port, database '$proxy_database', user '$user'"
echo "pgoverlay: the direct address $host:$port is reachable only from the branchd host (Docker) or from inside the cluster (Kubernetes)"
