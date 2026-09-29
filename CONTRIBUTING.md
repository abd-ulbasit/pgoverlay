# Contributing to pgoverlay

Thanks for taking the time. Bug reports, fixes, docs corrections and new tests
are all welcome. For anything larger than a bug fix, open an issue first so the
approach can be agreed before you write the code.

Security problems are the exception: do not open a public issue. Follow
[SECURITY.md](SECURITY.md) and report them privately.

## Development setup

You need:

- **Go**, the version in the `go` directive of [`go.mod`](go.mod). With the
  default `GOTOOLCHAIN=auto` an older local Go downloads the right toolchain
  on first use.
- **Docker** for the integration tests (any engine the `docker` CLI can reach:
  Docker Desktop, Colima, a remote engine through a docker context).
- **kind**, **kubectl** and **helm** for the Kubernetes tests.
- **Node 18+** for the JavaScript SDKs in `sdk/js` and `sdk/js-connect`.
- **jq** for `make vuln`.

```bash
git clone https://github.com/abd-ulbasit/pgoverlay
cd pgoverlay
make build   # bin/pgb, bin/branchd, bin/pgoverlay-github
make test    # unit tests
```

## Tests

The unit suite needs nothing but Go and is what every pull request must pass:

```bash
go build ./...
go vet ./...
test -z "$(gofmt -l .)"
go test -race ./...
```

`make check-toolchain` checks that the Dockerfiles build on the same Go version
as `go.mod`, `make helm-test` lints and renders the Helm chart, and
`make js-sdk-test` runs both JavaScript SDK suites.

### Integration tests

The integration suites are opt-in through environment variables, so a plain
`go test ./...` never needs Docker. They create real containers, kind clusters
and Helm releases, and clean up after themselves.

| Command | Gate | Needs |
|---|---|---|
| `make it` | `PGOVERLAY_IT=1` | Docker. Branch containers run with `CAP_SYS_ADMIN` for the overlay mount. |
| `make matrix` | `PGOVERLAY_MATRIX_IT=1` | Docker; pulls one `postgres:<major>` image per version (`PGOVERLAY_MATRIX_VERSIONS`, default `14 18`). |
| `make k8s-it` | `PGOVERLAY_K8S_IT=1` | Docker, kind, kubectl, helm. Creates the `pgoverlay-test` kind cluster. |
| `make csi-it` | `PGOVERLAY_CSI_IT=1` | As above, plus the CSI hostpath driver (`hack/kind-csi-up.sh`). |

The Makefile targets run test packages one at a time (`-p 1`), exactly like CI:
the Docker suites publish ephemeral host ports and the Kubernetes suites share
one kind cluster, so running packages in parallel makes them race each other.

Branch data paths are published on `127.0.0.1` of the Docker host. If your
engine is on another machine (a remote docker context), the integration tests
must run where they can reach that host's loopback, so run them on the engine's
host or in CI.

### What CI runs

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every push to
`main` and every pull request: unit tests with the race detector, the
govulncheck gate (`make vuln`), the Docker integration suite, the Helm chart
tests, both JavaScript SDK suites, both image builds, and the Kubernetes suites
on kind. A weekly scheduled run repeats the vulnerability scan and runs the
Postgres 14 to 18 version matrix. You can reproduce every job locally with the
Makefile target it calls.
[`.github/workflows/bench-cow.yml`](.github/workflows/bench-cow.yml) runs the
copy-on-write pgbench benchmark (`hack/bench-cow.sh`) on GitHub-hosted amd64
and arm64 runners; start it from the Actions tab or by pushing a branch named
`bench/<anything>` (see [docs/benchmarks.md](docs/benchmarks.md#running-it)).

## Commit style

Commits follow a lowercase conventional style:

```
fix(runtime): wait for the container to be gone before removing its volume
feat(api): add the branch history endpoint
test(deploy): wait for the pod to be ready before port-forwarding
docs(security): describe the accepted advisories
```

- The type is one of `feat`, `fix`, `test`, `docs`, `build`, `ci`, `deps`,
  `refactor` or `chore`; the scope is the package or area.
- Use the imperative mood, lowercase, no trailing period.
- Keep each commit to one logical change, and put the reasoning (why, not what)
  in the body.
- Behaviour changes come with a test that fails without the change.

## Pull requests

- Keep pull requests focused, and describe what changed and how you tested it
  (the pull request template has the checklist).
- The `/v1` REST API is a stability promise (see [docs/api.md](docs/api.md)):
  adding fields and endpoints is fine, removing or renaming them is not, and
  `internal/api/compat_test.go` enforces it.
- Update the docs in the same pull request when behaviour changes.

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), and you agree to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
