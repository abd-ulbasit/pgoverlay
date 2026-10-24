#!/usr/bin/env bash
# Installs trivy for CI (ci.yml `iac`, release.yml `scan`) on linux/amd64,
# from its release tarball, and refuses it unless two independent checks
# agree:
#
#   1. trivy_<v>_checksums.txt carries a Sigstore signature from trivy's own
#      release workflow at the v<v> tag (cosign verify-blob, keyless, so
#      cosign must already be installed). A file uploaded to the release
#      page by hand, with a stolen token, fails this.
#   2. The tarball's sha256 equals TRIVY_SHA256 below, which a reviewer
#      committed, and the signed checksums file lists the same value.
#
# Neither check catches a malicious release published through trivy's own
# release process. In the March 2026 incident (GHSA-69fq-xp46-6x23) a
# malicious v0.69.4 shipped with valid checksums, and the trivy-action and
# setup-trivy tags were force-pushed. Releases like that are usually found
# and pulled within days, so bump TRIVY_VERSION only to a release at least 7
# days old, unless it is a security fix you need sooner, and say which in the
# commit. trivy runs only in jobs whose token is read-only (no
# packages:write, no id-token), so a bad build could fake a scan result or
# read the repository, but not publish or sign anything.
#
# Pinned 2026-10-24: v0.76.1 (published 2026-10-21) is a security release,
# Go 1.27.3 with the eMudhra intermediate CA distrust; the previous release,
# v0.76.0, is 10 days old but lacks it.
set -euo pipefail

TRIVY_VERSION=0.76.1
TRIVY_SHA256=c7cbe3137fb8a48a17574bede3dd6beb16742d1e9772929940fc90082c39d9c0
TRIVY_SIGNER="https://github.com/aquasecurity/trivy/.github/workflows/reusable-release.yaml@refs/tags/v${TRIVY_VERSION}"

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) ;;
  *) echo "install-trivy: CI only (Linux x86_64); install trivy ${TRIVY_VERSION} yourself elsewhere" >&2; exit 1 ;;
esac
command -v cosign >/dev/null || { echo "install-trivy: cosign is required to verify the release" >&2; exit 1; }

dest=${1:-/usr/local/bin}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"

base="https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}"
sums="trivy_${TRIVY_VERSION}_checksums.txt"
tgz="trivy_${TRIVY_VERSION}_Linux-64bit.tar.gz"
curl -fsSL --retry 3 -O "$base/$sums" -O "$base/$sums.sigstore.json" -O "$base/$tgz"

cosign verify-blob "$sums" --bundle "$sums.sigstore.json" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity "$TRIVY_SIGNER"
grep -qxF "$TRIVY_SHA256  $tgz" "$sums" ||
  { echo "install-trivy: the signed $sums does not list TRIVY_SHA256 for $tgz" >&2; exit 1; }
printf '%s  %s\n' "$TRIVY_SHA256" "$tgz" | sha256sum --check --strict

tar -xzf "$tgz" trivy
if [ -w "$dest" ]; then mv trivy "$dest/trivy"; else sudo mv trivy "$dest/trivy"; fi
"$dest/trivy" --version
