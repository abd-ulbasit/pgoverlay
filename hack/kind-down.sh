#!/usr/bin/env bash
# Deletes the pgbranch-test kind cluster (and with it all pgbranch data under
# /var/lib/pgbranch inside the node container).
set -euo pipefail
kind delete cluster --name pgbranch-test
