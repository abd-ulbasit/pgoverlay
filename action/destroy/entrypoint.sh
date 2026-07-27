#!/usr/bin/env bash
# Delete a pgoverlay branch by name. 404 (already gone — TTL reaper or an
# earlier delete) counts as success. Tested by internal/actiontest.
set -euo pipefail

: "${PGOVERLAY_SERVER:?PGOVERLAY_SERVER (input server) is required}"
: "${PGOVERLAY_TOKEN:?PGOVERLAY_TOKEN (input token) is required}"
: "${PGOVERLAY_BRANCH:?PGOVERLAY_BRANCH (input branch) is required}"

server="${PGOVERLAY_SERVER%/}"
name="$PGOVERLAY_BRANCH"

code="$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE \
  -H "Authorization: Bearer $PGOVERLAY_TOKEN" "$server/v1/branches/$name")"
case "$code" in
  204) echo "pgoverlay: branch '$name' destroyed" ;;
  404) echo "pgoverlay: branch '$name' already gone" ;;
  *)
    echo "destroy branch '$name' failed: HTTP $code" >&2
    exit 1
    ;;
esac
