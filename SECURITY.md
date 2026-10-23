# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability
reporting: open
[**Report a vulnerability**](https://github.com/abd-ulbasit/pgoverlay/security/advisories/new)
(Security tab → Advisories). Please do not open a public issue, pull request
or discussion for an undisclosed vulnerability.

A useful report says which component is affected (`pgb`, `branchd`, the
`/v1` API, the Postgres router, the GitHub webhook service, the Helm chart or
the GitHub Action), the version or commit, how to reproduce it, and what an
attacker gains. The fix is developed in the private advisory, released, and
then the advisory is published with a CVE where one applies, crediting the
reporter unless you ask otherwise.

## Supported versions

Security fixes land on `main` and ship in the next release. Only the newest
release is supported; nothing is backported to older tags, and a pre-release
(`-rc`) is superseded by the release that follows it. Run the newest
[release](https://github.com/abd-ulbasit/pgoverlay/releases/latest).

The `/v1` REST contract in [docs/api.md](docs/api.md) is stable from v1.0.0
onwards and CI-enforced (`internal/api/compat_test.go`): within v1, fields and
endpoints are only ever added.

**The GitHub Action's major tag (`action@v1`) moves.** Following the Actions
convention, the release workflow points `v1` at the newest stable v1.x.y
release each time one is published, so `action@v1` runs different code over
time. Pin a full release tag or a commit SHA if you need an immutable
reference, as you would for any third-party action.

### The pgbranch → pgoverlay rename

This project was called `pgbranch` up to and including `v1.0.0-rc.3`. From
`v1.0.0-rc.4` the name, the Go module path
(`github.com/abd-ulbasit/pgoverlay`), every `PGOVERLAY_*` environment variable,
the container/pod labels, the Prometheus metric names, the on-disk state paths,
and the Helm chart all use `pgoverlay`. The `pgb` CLI and the `branchd` daemon
keep their names. Nothing is backported to the `pgbranch` tags, and there is no
automatic migration: a deployment created by a `pgbranch` release is not seen
by a `pgoverlay` release (different state directory, registry filename, labels,
and resource names). Destroy branches with the old version before upgrading.

One rename fails quietly rather than loudly and is worth calling out: the
GitHub App posts its commit status under the context `pgoverlay/branch`, which
was `pgbranch/branch`. A branch protection rule that lists `pgbranch/branch` as
a **required** status check will never be satisfied again — the new context is
a different check, so pull requests wait forever instead of erroring. Update
the rule at the same time you upgrade ghook.

## Release integrity

Every release is built by `.github/workflows/release.yml` from the tagged
commit, after the full CI suite passes at that commit:

- Binaries (`pgb`, `branchd`, `pgoverlay-github`; linux and darwin, amd64 and
  arm64) are published with a `checksums.txt` and a signed build provenance
  attestation. Verify an archive with
  `sha256sum --check --ignore-missing checksums.txt`, and its provenance with
  `gh attestation verify <archive> --repo abd-ulbasit/pgoverlay`.
- The images `ghcr.io/abd-ulbasit/pgoverlay-branchd` and
  `ghcr.io/abd-ulbasit/pgoverlay-ghook` are multi-arch (linux/amd64,
  linux/arm64), tagged with the release tag, and carry an SBOM and build
  provenance. Their base images are pinned by digest. Before anything is
  published, binaries or images, the `scan` job builds each image for each
  platform and scans it with Trivy, and the release stops on a HIGH or
  CRITICAL vulnerability in an OS package that has a fix (Go code is gated
  by `govulncheck`, below). Once pushed, each image is signed
  with cosign (keyless, through GitHub's OIDC identity) and gets a SLSA
  provenance attestation stored in the registry next to it, both bound to
  the digest. Verify an image, then pin the verified digest (the Helm
  chart's `image.digest`):

  ```bash
  IMG=ghcr.io/abd-ulbasit/pgoverlay-branchd
  DIGEST=$(docker buildx imagetools inspect "$IMG:<tag>" --format '{{.Manifest.Digest}}')
  cosign verify "$IMG@$DIGEST" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --certificate-identity "https://github.com/abd-ulbasit/pgoverlay/.github/workflows/release.yml@refs/tags/<tag>"
  gh attestation verify "oci://$IMG@$DIGEST" --repo abd-ulbasit/pgoverlay
  ```

  Images published before this was added (v1.0.0 and earlier) are not
  signed and have no registry attestation.

Every publishing job runs in the `release` GitHub environment, so that the
environment's protection rules, not only who can push a tag, decide whether
a release ships. **Those rules are repository settings and are not
configured yet.** Until they are, GitHub creates `release` with no rules on
the first tag push, and anyone who can push a `v*` tag can publish a release
and move `v1`. The owner needs to:

- create the `release` environment with a required reviewer and a
  deployment-tag rule that admits only `v*` tags;
- add a tag ruleset for `v*.*.*` that limits who can create those tags and
  blocks updating and deleting them (`v1` itself must stay movable by the
  release workflow, so it is not in the pattern);
- then do a dry run with an `-rc` tag: the scan, signing, attestation and
  environment steps have not run in a real release yet.

## Supply-chain scanning

[`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) runs in
binary mode against the three shipped binaries (the `vuln` CI job) on every
push to `main`, on every pull request, on every release tag before anything is
published, and on a weekly schedule, so an advisory published against released
code fails a run within a week even when nobody pushes. The job is a thin
wrapper around [`hack/vulncheck.sh`](hack/vulncheck.sh), which is also
`make vuln`, so it can be run locally before pushing.

The gate:

- fails on any vulnerability whose vulnerable code is reachable from a shipped
  binary, standard library included, unless that advisory ID is listed in
  [`hack/vuln-allowlist.txt`](hack/vuln-allowlist.txt);
- **fails closed**: if govulncheck errors, cannot reach the vulnerability
  database, or prints anything but a complete result, the job fails rather
  than reporting a clean scan (`make vuln-test` checks this with a stub
  scanner);
- pins the govulncheck version, so a scanner release cannot change the gate
  without a commit.

The build toolchain is pinned by the `go` directive in `go.mod`, and
`make check-toolchain` keeps the Dockerfiles' base images on the same version;
bumping it is how standard-library advisories are cleared. The full unit suite
also runs under the Go race detector.

Two more gates run on every pull request and push, and the second weekly:

- **Secrets** (the `secrets` job, `make secrets`): gitleaks over the whole git
  history with its default rules, which include generic API keys, private
  keys and high-entropy assignments, not only the provider token formats
  GitHub's secret scanning recognises. Test placeholders are allowlisted in
  [`.gitleaks.toml`](.gitleaks.toml), each by path and pattern together.
- **Misconfiguration** (the `iac` job, `make misconfig` and `make tf-test`):
  `trivy config` over the Helm chart, rendered with the default values,
  with ghook enabled (it is off by default) and with the production values,
  and over the EKS Terraform, failing on HIGH and
  CRITICAL; accepted findings, each with a reason and an expiry, are in
  [`.trivyignore.yaml`](.trivyignore.yaml). The Terraform is also
  format-checked, validated and tested offline against a mocked AWS
  provider.

**Committed binaries.** Two small C programs ship prebuilt so that
`go install` needs no C toolchain: the lazyrw `LD_PRELOAD` shim
(`internal/cow/lazyrw/dist`, four builds: glibc and musl, x86_64 and aarch64)
and the static `pgoverlay-du` usage tool (`internal/cow/usage/dist`, x86_64
and aarch64). Each directory has a `SHA256SUMS`; the Go code embeds them and
refuses a build that does not match it, and the overlay install helper checks
each shim build's SHA-256 again after decoding it in the helper. The builds
are reproducible: `make lazyrw` rebuilds them in digest-pinned Debian and
Alpine images with `docker buildx`, and the `lazyrw` CI job (`make
lazyrw-check`) rebuilds them from source on every pull request and fails on
any byte of difference. The `pg-import-audit` job checks, for the `postgres`
binaries of `postgres:14` to `18` and `17-alpine` and weekly, that every
write-class libc function they import is interposed by the shim or on a
reviewed list, since a write the shim does not see would hit a read-only file
descriptor.

## What the automated checks cover, and what they do not

| Layer | Check | Catches | Does not catch |
|---|---|---|---|
| Go code in `pgb`, `branchd`, `pgoverlay-github` | `vuln` (govulncheck, binary mode), every push, PR, release and weekly | known advisories in code reachable from the binaries, standard library included | advisories in unreachable code (deliberately), anything not yet in the Go vulnerability database |
| The two release images | release `scan` job (Trivy, before anything is published) | HIGH and CRITICAL vulnerabilities with a fix in their Alpine packages | unfixed and lower-severity ones; anything published after the release (images are scanned when built, so a later CVE waits for the next release) |
| Images branchd runs: `postgres:<major>` or a source's `--image`, a mirrored helper image | nothing in this repository | | everything: they are the operator's choice; scan and pin them where they are mirrored ([docs/security.md](docs/security.md#branch-instances)) |
| Secrets in commits | `secrets` (gitleaks, full history) | provider tokens, private keys, high-entropy assignments | low-entropy passwords, and secrets outside git: CI logs, Helm release history, container environments |
| Helm chart and Terraform | `iac` (`trivy config`, rendered chart and Terraform), `helm-test`, `tf-test` | known misconfiguration patterns at HIGH and CRITICAL, the chart's hardening fields, invalid Terraform inputs | the branch and helper pods branchd builds at runtime in Go (covered by unit tests in `internal/runtime` and `internal/deploy`), and anything about a live cluster or account |
| Committed C binaries | `lazyrw` (byte-for-byte rebuild), `pg-import-audit` | any difference from the source; write paths the shim does not interpose | a compromised toolchain image (pinned by digest; the glibc one is Debian 11, past its LTS) |
| Release integrity | checksums, provenance and cosign signatures | tampering after the build, and where an artifact came from | a bad change merged to `main` or a tag pushed by someone with write access: that is branch protection, tag rules and the `release` environment's reviewers, which are repository settings (the last two not configured yet) |
| Data in branches | nothing automated | | whether masking covers every sensitive column, and what masking leaves behind ([docs/security.md](docs/security.md#what-masking-leaves-behind)) |

## Accepted advisories

The allowlist names individual advisory IDs, never a whole module, so a new
advisory fails the build until someone decides about it here. Each entry has
an expiry date, and the gate fails when an entry expires or when its advisory
gains a fixed version on the listed module.

| ID | Module | Expires | What it is | Why it is accepted |
|----|--------|---------|------------|--------------------|
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) (CVE-2026-34040) | `github.com/docker/docker` v28.5.2 | 2026-12-31 | AuthZ plugin bypass when the Docker daemon receives an oversized request body. | A flaw in the daemon's authorization-plugin path. pgoverlay only runs the Docker **client** to manage its own containers; the vulnerability database attributes the advisory to the whole module, which is why the client symbols are flagged. |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) (CVE-2026-33997) | `github.com/docker/docker` v28.5.2 | 2026-12-31 | Off-by-one in the daemon's plugin privilege validation. | Daemon-side plugin installation. pgoverlay runs only the client and installs no plugins. |

govulncheck also reports three more `github.com/docker/docker` advisories as
present in a linked module but not reachable from pgoverlay's code, so they do
not gate: [GO-2026-5617](https://pkg.go.dev/vuln/GO-2026-5617) and
[GO-2026-5668](https://pkg.go.dev/vuln/GO-2026-5668) (races in the daemon's
`docker cp` that allow bind-mount redirection and arbitrary file creation) and
[GO-2026-5746](https://pkg.go.dev/vuln/GO-2026-5746) (the daemon's
`PUT /containers/{id}/archive` executing a container binary on the host).
pgoverlay never copies files into or out of a container: branch data reaches a
container through the OverlayFS mount assembled in its own mount namespace
(`internal/cow/entrypoint.sh`), and seeding uses `pg_basebackup` against a
running source.

### How the allowlist ends

None of these advisories has a fix on `github.com/docker/docker`, and none
will: that module path has published nothing since v28.5.2+incompatible, and
Moby now ships from `github.com/moby/moby/v2` (daemon) and
`github.com/moby/moby/client` / `github.com/moby/moby/api` (client). The
client modules are not affected by any of the advisories above. pgoverlay uses
`github.com/docker/docker` from one file, `internal/runtime/docker.go`;
porting it to `github.com/moby/moby/client` removes the module and empties the
allowlist. The 2026-12-31 expiry is the deadline for that port: past it, the
`vuln` job fails on its weekly run until the port lands or someone re-reviews
and re-dates the entries.

## Dependency updates

Dependabot opens weekly grouped update PRs for Go modules, GitHub Actions
(pinned by commit SHA) and the Dockerfiles' base images
([`.github/dependabot.yml`](.github/dependabot.yml)). The Go toolchain is the
exception: Dependabot cannot bump the `go` directive, so standard-library fixes
are applied by hand, and `make check-toolchain` keeps the Dockerfiles' base
images from drifting away from it.

Dependabot **alerts** are on. Dependabot **security updates** (a PR per alert)
are off: every open alert is one of the `github.com/docker/docker` advisories
above, none has a fixed version on that module path, and each attempt ends in
`security_update_not_found` and a failed job that can never pass. The alerts
stay visible on the Security tab, and the `vuln` job keeps gating.

## Hardening posture

[docs/security.md](docs/security.md) is the threat model (who can reach what,
what each component trusts) and the hardening checklist;
[docs/kubernetes.md](docs/kubernetes.md) covers the pod securityContext,
NetworkPolicy, RBAC and CSI vs hostPath, and [docs/api.md](docs/api.md) the
`/v1` routes, roles and stability promise. Notable defaults:

- Every `/v1` route is role-gated (viewer, operator, admin). branchd refuses
  to start with a `PGOVERLAY_TOKEN` shorter than 16 characters; stored tokens
  are kept as SHA-256 digests, and their names cannot impersonate the built-in
  token (`root`) or the system actors in the audit log.
- Rotated branch passwords are encrypted at rest with AES-256-GCM under a
  dedicated random key with a key id (`secret.key` in the state directory, or
  `PGOVERLAY_SECRET_KEY`), independent of `PGOVERLAY_TOKEN`, so the token can
  be rotated without touching them. A password no configured key can decrypt
  is reported as `password_unavailable` instead of breaking the branch, and a
  destroyed branch keeps no password.
- The state directory is created `0700`, and the registry and key files
  `0600`; older installs are tightened on start.
- An audit trail records the acting identity for every branch and source
  transition (`name (role)` for tokens, `root` for the built-in token,
  `local:<user>` for local-mode `pgb`, `system:reconcile` for the daemon),
  and outlives the source (`pgb history`).
- The GitHub webhook verifies HMAC-SHA256 with a secret of at least 16
  characters and de-duplicates deliveries.
- `make helm-test` asserts the chart's container hardening (no privilege
  escalation, all capabilities dropped, RuntimeDefault seccomp, read-only
  root filesystem, and a non-root user for the webhook service).

Branch containers are the exception to least privilege: on Docker and in
Kubernetes hostpath mode they run with `CAP_SYS_ADMIN` and AppArmor
unconfined for their overlay mount. See
[docs/security.md](docs/security.md#branch-instances). The lazyrw shim adds no
privilege: it is preloaded only inside the branch container and is active only
in its `postgres` server process.

On hosts where branch volumes sit on XFS with `reflink=1`, run a kernel with
the fix for CVE-2026-64600, an XFS copy-on-write race that lets a local user
write into a file they can only read; in pgoverlay that could be the shared
seed. Details in
[docs/security.md](docs/security.md#xfs-reflink-hosts-cve-2026-64600).
