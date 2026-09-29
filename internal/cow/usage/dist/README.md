# pgoverlay-du binaries

This directory holds the static `pgoverlay-du` binaries that pgoverlay embeds
(`internal/cow/usage.go`, `//go:embed usage/dist`) and runs inside helper
containers: to measure a branch's exclusive bytes on reflink filesystems, and
to set the XFS copy-on-write extent size hint on a volume root.

The files are built from `../pgoverlay-du.c` by `make lazyrw`
(`hack/build-lazyrw.sh`, `du` stage of `internal/cow/lazyrw/Dockerfile`). The
build is static against musl on the pinned Alpine toolchain, and it is
reproducible: CI's `make lazyrw-check` rebuilds the binaries and fails on any
byte of difference, so commit what `make lazyrw` writes here after changing
the source.

| File | Target |
|---|---|
| `pgoverlay-du-x86_64` | linux/amd64, static |
| `pgoverlay-du-aarch64` | linux/arm64, static |
| `SHA256SUMS` | `sha256sum` lines for the two binaries |

The names use `uname -m`, which is what the helper reports.

Keep each binary small (a musl static build is well under 100 KiB): it travels
to the helper base64-encoded in environment variables, and a binary larger
than `cow.MaxDuToolSize` is ignored.

A binary whose SHA-256 does not match its `SHA256SUMS` line is ignored too.
When no usable binary exists for the helper's architecture, pgoverlay falls
back to `du -sb`, which counts reflinked extents as the branch's own: usage is
over-reported on XFS and btrfs hosts, never under-reported.
