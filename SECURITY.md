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
  provenance. Their base images are pinned by digest.

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
[docs/security.md](docs/security.md#branch-instances).
