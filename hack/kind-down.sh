#!/usr/bin/env bash
# Deletes the pgoverlay-test kind cluster (and with it all pgoverlay data under
# /var/lib/pgoverlay inside the node container).
set -euo pipefail
kind delete cluster --name pgoverlay-test
