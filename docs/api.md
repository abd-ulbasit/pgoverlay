# REST API stability (`/v1`)

branchd's REST API is versioned in the path (`/v1/...`). From v1.0 it is a
**backward-compatibility promise**:

- **Additive only.** New endpoints, new optional request fields, and new
  response fields may be added at any time within `/v1`. Clients must ignore
  response fields they don't recognise.
- **No silent breaks.** A documented response field is never renamed or
  removed, an endpoint never changes its method/path, and a required request
  field is never added, without bumping to `/v2`. Both versions would then be
  served during a deprecation window.
- **Enforced in CI.** `internal/api/compat_test.go` locks the documented JSON
  field set of every response type (`Branch`, `Source`, `Token`,
  `DiffResult`/`TableDelta`, `ReconcilePlan`/`Action`); a rename or removal
  fails the build.

Authentication is unchanged across `/v1`: `Authorization: Bearer <token>`,
with role-based scoping (see [Kubernetes](kubernetes.md) and
`pgb token`). `/healthz`, `/readyz`, and `/metrics` are unauthenticated and
not part of the versioned contract.

Endpoints: see the [Quickstart](quickstart.md) (REST section) for the full
list — sources, branches, reset, diff, reconcile, and tokens.
