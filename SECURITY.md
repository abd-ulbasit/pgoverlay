# Security Policy

## Reporting a vulnerability

Open a private security advisory on GitHub (Security → Advisories → *Report a
vulnerability*) or email the maintainer. Please do not file public issues for
undisclosed vulnerabilities.

## Supported versions

Fixes land on `main` and ship in the next release; nothing is backported to
older tags. Run the newest published
[release](https://github.com/abd-ulbasit/pgoverlay/releases).

There is no stable `v1.0.0` yet — the current line is `v1.0.0-rc`, and the last
stable tag is `v0.3.0`. The `/v1` REST contract in `docs/api.md` is written and
CI-enforced (`internal/api/compat_test.go`), but it is a promise **from v1.0**;
until that tag exists, treat the CLI flags, the Helm values, and the API alike
as pre-1.0.

**The GitHub Action's `v1` tag is a floating pointer, not a stable release.**
It was published with `v1.0.0-rc.4`, the first tag of this repository to carry
the `pgoverlay` action at all — `v0.2.0`, `v0.3.0` and `v1.0.0-rc.1` …
`v1.0.0-rc.3` all predate the rename and carry the old `pgbranch` action, so
none of them work under the new name. Following the Actions convention, `v1`
moves to the newest compatible release, which means `action@v1` is a moving
target in the same way `@main` is. Pin a full tag (`action@v1.0.0-rc.4`) or a
commit SHA if you need an immutable reference, as you would for any
third-party action. `v1` carries no API-stability promise, and its existence
does not mean a stable `v1.0.0` of the project exists — see above.

### The pgbranch → pgoverlay rename

This project was called `pgbranch` up to and including `v1.0.0-rc.3`. From
`v1.0.0-rc.4` the name, the Go module path
(`github.com/abd-ulbasit/pgoverlay`), every `PGOVERLAY_*` environment variable,
the container/pod labels, the Prometheus metric names, the on-disk state paths,
and the Helm chart all use `pgoverlay`. The `pgb` CLI and the `branchd` daemon
keep their names. Nothing is backported to the `pgbranch` tags, and there is no
automatic migration — a deployment created by an older tag will not be seen by
`v1.0.0-rc.4` (different state directory, registry filename, labels, and
resource names). Destroy branches with the old version before upgrading.

One rename fails quietly rather than loudly and is worth calling out: the
GitHub App posts its commit status under the context `pgoverlay/branch`, which
was `pgbranch/branch`. A branch protection rule that lists `pgbranch/branch` as
a **required** status check will never be satisfied again — the new context is
a different check, so pull requests wait forever instead of erroring. Update
the rule at the same time you upgrade ghook.

## Supply-chain scanning

CI runs [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) in
binary mode against the compiled `branchd` and `pgoverlay-github` images on every
push and pull request (the `vuln` job), and the full unit suite runs under the
Go race detector (`go test -race`). The build toolchain is pinned via the `go`
directive in `go.mod`; bumping it is how stdlib CVEs are cleared.

The `vuln` job fails on any reachable vulnerability **except** the documented
allowlist below. It is a thin wrapper around
[`hack/vulncheck.sh`](hack/vulncheck.sh), which is also `make vuln` — the gate
is one script, so it can be run locally before pushing rather than discovered
in CI.

## Accepted upstream advisories

| ID | Module | Status | Why accepted |
|----|--------|--------|--------------|
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) | `github.com/docker/docker` v28.5.2 | No fix on this module path | Moby plugin-privilege validation. pgoverlay uses the Docker **client** only to manage branch containers and **installs no Docker plugins**, so the affected code path is not exercised. |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) | `github.com/docker/docker` v28.5.2 | No fix on this module path | Same as above — plugin-privilege off-by-one; unreachable in pgoverlay's usage. |
| [GO-2026-5617](https://pkg.go.dev/vuln/GO-2026-5617) | `github.com/docker/docker` v28.5.2 | No fix on this module path | Race in `docker cp` allowing bind-mount redirection to a host path. pgoverlay never calls `docker cp`: branch data reaches a container through the OverlayFS mount assembled in its own mount namespace (`internal/cow/entrypoint.sh`), and seeding uses `pg_basebackup` against a running source. |
| [GO-2026-5668](https://pkg.go.dev/vuln/GO-2026-5668) | `github.com/docker/docker` v28.5.2 | No fix on this module path | Another plugin-privilege off-by-one; unreachable for the same reason as GO-2026-4887. |

### Why the allowlist is scoped to a module, not a list of IDs

Moby publishes plugin-privilege and `docker cp` advisories faster than it
publishes fixed releases on the `github.com/docker/docker` module path: four of
them affect v28.5.2, and none has a fixed version there. An ID list meant CI
went red every time a new one landed, which trains you to ignore the one job
whose whole purpose is to be believed.

The `vuln` job therefore allows advisories **whose module is
`github.com/docker/docker`** and fails on everything else. It uses govulncheck's
text output to decide which advisories count for a binary, then its JSON output
only to map each of those IDs back to a module. (In `-mode=binary` the JSON
lists every advisory affecting every linked module, which is a much larger set
and the wrong thing to gate on.)

This is deliberately narrower than "ignore known IDs" and deliberately wider
than one ID at a time. A stdlib or `golang.org/x/*` advisory still fails the
build: `GO-2026-5856` (Encrypted Client Hello privacy leak in `crypto/tls`) did
exactly that, and was fixed by moving to Go 1.26.5 rather than by allowlisting
it.

### When the allowlist expires — and what watches for it

"No fixed release" above means *no fixed release on the module path pgoverlay
depends on*. Upstream has fixed all four, but only in the **renamed** module
`github.com/moby/moby/v2` (v2.0.0-beta.8 and v2.0.0-beta.14), which is still a
beta. `github.com/docker/docker` itself has published nothing since
v28.5.2+incompatible (2025-11-05), so taking the fixes today would mean moving
to a pre-release under a new module path — a bigger change than the advisories
justify, given none of the affected code is reachable from pgoverlay.

That promise is not left to anyone's memory. `hack/vulncheck.sh` reads each
allowlisted advisory's OSV affected-ranges for `github.com/docker/docker` and
**fails the build** the day one of them gains a `fixed` version, with the
message to bump the dependency and delete the row from the table above. The
same script is `make vuln`, so it behaves identically on a laptop.

Everything else here is manual and worth saying plainly: the four rows above
are hand-written, and nothing checks that the *reasons* in the last column are
still true. If pgoverlay ever starts installing Docker plugins or calling
`docker cp`, this table becomes wrong and no job will notice.

Dependency updates themselves are automated — Dependabot **alerts** are on, with
a weekly `gomod` / `github-actions` / `docker` schedule in
[`.github/dependabot.yml`](.github/dependabot.yml). The Go toolchain is the
exception: Dependabot cannot bump the `go` directive, so stdlib CVEs are still
cleared by hand (that is how `GO-2026-5856` was), and `make check-toolchain`
keeps the Dockerfiles' base image from drifting away from it.

Dependabot **security updates** — the feature that opens a PR per alert — are
deliberately off, for the same reason the allowlist is scoped to a module rather
than a list of IDs. Every open alert on this repository is one of the
`github.com/docker/docker` rows above, and none of them has a fixed version on
that module path, so each attempt ends in
`security_update_not_found` and a failed job. That is a job nobody can ever make
green, on a repository whose security story depends on its red marks meaning
something. Two such failures were enough to demonstrate it.

Nothing is hidden by this. The alerts stay visible on the Security tab, the
`vuln` job still gates every push, and `hack/vulncheck.sh` still fails the build
the day any of these four gains a fixed release. Turning the PR opener back on
is one API call once `github.com/docker/docker` ships again, or once pgoverlay
moves to `github.com/moby/moby/v2`.

## Hardening posture

See `docs/kubernetes.md` (pod securityContext, NetworkPolicy, RBAC, CSI vs
hostPath) and `docs/api.md` (the `/v1` stability promise). Notable defaults:
branch passwords are AES-256-GCM encrypted at rest, every `/v1` route is
role-gated, the GitHub webhook verifies HMAC-SHA256, and an audit trail records
the acting token for every branch transition (`pgb history`).
