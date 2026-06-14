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

Both are tracked; the allowlist will be removed and the dependency bumped once
Moby ships a fixed release. v28.5.2 is currently the latest published version.

## Hardening posture

See `docs/kubernetes.md` (pod securityContext, NetworkPolicy, RBAC, CSI vs
hostPath) and `docs/api.md` (the `/v1` stability promise). Notable defaults:
branch passwords are AES-256-GCM encrypted at rest, every `/v1` route is
role-gated, the GitHub webhook verifies HMAC-SHA256, and an audit trail records
the acting token for every branch transition (`pgb history`).
