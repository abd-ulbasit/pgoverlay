#!/usr/bin/env bash
# Delete a pgbranch branch by name. 404 (already gone — TTL reaper or an
# earlier delete) counts as success. Tested by internal/actiontest.
set -euo pipefail

: "${PGBRANCH_SERVER:?PGBRANCH_SERVER (input server) is required}"
: "${PGBRANCH_TOKEN:?PGBRANCH_TOKEN (input token) is required}"
: "${PGBRANCH_BRANCH:?PGBRANCH_BRANCH (input branch) is required}"

server="${PGBRANCH_SERVER%/}"
name="$PGBRANCH_BRANCH"

code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $PGBRANCH_TOKEN" "$server/v1/branches/$name")"
case "$code" in
  204) echo "pgbranch: branch '$name' destroyed" ;;
  404) echo "pgbranch: branch '$name' already gone" ;;
  *)
    echo "destroy branch '$name' failed: HTTP $code" >&2
    exit 1
    ;;
esac
