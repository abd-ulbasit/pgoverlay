#!/usr/bin/env bash
# Create a pgoverlay branch and wait until it is ready. Used by action.yml,
# tested by internal/actiontest against a stub server.
#
# In:  PGOVERLAY_SERVER (required), PGOVERLAY_TOKEN (required),
#      PGOVERLAY_SOURCE (default main), PGOVERLAY_BRANCH (default generated),
#      PGOVERLAY_TTL (seconds, default 3600),
#      PGOVERLAY_POLL_MAX / PGOVERLAY_POLL_INTERVAL (default 60 x 5s).
# Out: branch/host/port/database appended to $GITHUB_OUTPUT.
#      The token is never echoed and the password is never an output.
set -euo pipefail

: "${PGOVERLAY_SERVER:?PGOVERLAY_SERVER (input server) is required}"
: "${PGOVERLAY_TOKEN:?PGOVERLAY_TOKEN (input token) is required}"

server="${PGOVERLAY_SERVER%/}"
source="${PGOVERLAY_SOURCE:-main}"
ttl="${PGOVERLAY_TTL:-3600}"
name="${PGOVERLAY_BRANCH:-}"
poll_max="${PGOVERLAY_POLL_MAX:-60}"
poll_interval="${PGOVERLAY_POLL_INTERVAL:-5}"

case "$ttl" in
  ''|*[!0-9]*) echo "ttl must be a non-negative integer (seconds), got '$ttl'" >&2; exit 1 ;;
esac

if [ -z "$name" ]; then
  rand="$(od -An -tx1 -N3 /dev/urandom | tr -d ' \n')"
  name="t-gha-${GITHUB_RUN_ID:-0}-${rand}"
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
  echo "create branch '$name' failed: HTTP $code: $payload" >&2
  exit 1
fi

# the create endpoint is synchronous today, but poll GET regardless
state="$(jq -r '.state // empty' <<<"$payload")"
tries=0
while [ "$state" != "ready" ]; do
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

host="$(jq -r '.host // empty' <<<"$payload")"
port="$(jq -r '.port // empty' <<<"$payload")"
database="$(jq -r '.database // empty' <<<"$payload")"

{
  echo "branch=$name"
  echo "host=$host"
  echo "port=$port"
  echo "database=$database"
} >>"$GITHUB_OUTPUT"

echo "pgoverlay: branch '$name' ready at $host:$port/$database"
