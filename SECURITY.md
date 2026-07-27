# Security Policy

## Reporting a vulnerability

Open a private security advisory on GitHub (Security → Advisories → *Report a
vulnerability*) or email the maintainer. Please do not file public issues for
undisclosed vulnerabilities.

## Supported versions

The latest `v1.x` release line receives security fixes.

## Supply-chain scanning

CI runs [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) in
binary mode against the compiled `branchd` and `pgbranch-github` images on every
push and pull request (the `vuln` job), and the full unit suite runs under the
Go race detector (`go test -race`). The build toolchain is pinned via the `go`
directive in `go.mod`; bumping it is how stdlib CVEs are cleared.

The `vuln` job fails on any reachable vulnerability **except** the documented
allowlist below.

## Accepted upstream advisories

| ID | Module | Status | Why accepted |
|----|--------|--------|--------------|
| [GO-2026-4887](https://pkg.go.dev/vuln/GO-2026-4887) | `github.com/docker/docker` v28.5.2 | No fixed release | Moby plugin-privilege validation. pgbranch uses the Docker **client** only to manage branch containers and **installs no Docker plugins**, so the affected code path is not exercised. |
| [GO-2026-4883](https://pkg.go.dev/vuln/GO-2026-4883) | `github.com/docker/docker` v28.5.2 | No fixed release | Same as above — plugin-privilege off-by-one; unreachable in pgbranch's usage. |
| [GO-2026-5617](https://pkg.go.dev/vuln/GO-2026-5617) | `github.com/docker/docker` v28.5.2 | No fixed release | Race in `docker cp` allowing bind-mount redirection to a host path. pgbranch never calls `docker cp`: branch data reaches a container through the OverlayFS mount assembled in its own mount namespace (`internal/cow/entrypoint.sh`), and seeding uses `pg_basebackup` against a running source. |
| [GO-2026-5668](https://pkg.go.dev/vuln/GO-2026-5668) | `github.com/docker/docker` v28.5.2 | No fixed release | Another plugin-privilege off-by-one; unreachable for the same reason as GO-2026-4887. |

### Why the allowlist is scoped to a module, not a list of IDs

Moby publishes plugin-privilege and `docker cp` advisories faster than it
publishes fixed releases: four of them affect v28.5.2, and none has a fixed
version. An ID list meant CI went red every time a new one landed, which trains
you to ignore the one job whose whole purpose is to be believed.

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

The Docker allowlist will be removed and the dependency bumped once Moby ships
a fixed release. v28.5.2 is currently the latest published version.

## Hardening posture

See `docs/kubernetes.md` (pod securityContext, NetworkPolicy, RBAC, CSI vs
hostPath) and `docs/api.md` (the `/v1` stability promise). Notable defaults:
branch passwords are AES-256-GCM encrypted at rest, every `/v1` route is
role-gated, the GitHub webhook verifies HMAC-SHA256, and an audit trail records
the acting token for every branch transition (`pgb history`).
