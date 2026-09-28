## What and why

<!-- What does this change, and what problem does it solve? Link the issue: "Fixes #123". -->

## How it was tested

<!-- Commands you ran and what they showed. Name any integration suite you ran (make it, make k8s-it, make matrix). -->

## Checklist

- [ ] `go build ./...`, `go vet ./...` and `gofmt -l .` are clean
- [ ] `go test -race ./...` passes
- [ ] Behaviour changes have a test that fails without the change
- [ ] Docs are updated where behaviour or flags changed
- [ ] The `/v1` API change, if any, is additive (`internal/api/compat_test.go` passes)
- [ ] Commits follow the style in CONTRIBUTING.md
