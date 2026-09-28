#!/usr/bin/env bash
# Delete a pgoverlay branch by name. Tested by internal/actiontest.
#
# - An empty branch name is a no-op with a warning: the create action only
#   emits `branch` once the server has created one, so empty means there is
#   nothing to delete (the create step failed first).
# - A name that is not a valid branch name fails before any request, so a raw
#   git ref ("feat/login") cannot hit a different route and "succeed".
# - 204 is success; 404 counts as "already gone" (TTL reaper or an earlier
#   delete) only when branchd itself says so (a JSON error body). Anything
#   else, including a 404 from something that is not the branchd API, fails.
set -euo pipefail

: "${PGOVERLAY_SERVER:?PGOVERLAY_SERVER (input server) is required}"
: "${PGOVERLAY_TOKEN:?PGOVERLAY_TOKEN (input token) is required}"

server="${PGOVERLAY_SERVER%/}"
name="${PGOVERLAY_BRANCH:-}"
name_re='^[a-z0-9][a-z0-9-]{0,40}$'

if [ -z "$name" ]; then
  echo "::warning title=pgoverlay destroy::no branch name given, nothing to destroy (did the create step fail before creating a branch?)"
  exit 0
fi
if ! [[ "$name" =~ $name_re ]]; then
  echo "invalid branch name '$name': must match $name_re (pass the create action's branch output, not a git ref)" >&2
  exit 1
fi

resp="$(curl -sS -w '\n%{http_code}' -X DELETE \
  -H "Authorization: Bearer $PGOVERLAY_TOKEN" "$server/v1/branches/$name")"
code="${resp##*$'\n'}"
payload="${resp%$'\n'*}"
case "$code" in
  204) echo "pgoverlay: branch '$name' destroyed" ;;
  404)
    if jq -e 'type == "object" and (.error | type == "string")' >/dev/null 2>&1 <<<"$payload"; then
      echo "pgoverlay: branch '$name' already gone"
    else
      echo "destroy branch '$name': HTTP 404 without a branchd error body (is '$server' the branchd API?): $payload" >&2
      exit 1
    fi
    ;;
  *)
    echo "destroy branch '$name' failed: HTTP $code: $payload" >&2
    exit 1
    ;;
esac
