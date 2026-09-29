# pgoverlay-du binaries

This directory holds the static `pgoverlay-du` binaries that pgoverlay embeds
(`internal/cow/usage.go`, `//go:embed usage/dist`) and runs inside helper
containers: to measure a branch's exclusive bytes on reflink filesystems, and
to set the XFS copy-on-write extent size hint on a volume root.

Expected files, built from `../pgoverlay-du.c` by the lazyrw build script:

| File | Target |
|---|---|
| `pgoverlay-du-x86_64` | linux/amd64, static |
| `pgoverlay-du-aarch64` | linux/arm64, static |
| `SHA256SUMS` | `sha256sum` lines for the two binaries |

The names use `uname -m`, which is what the helper reports. Build them static
against musl, for example on `alpine` with `apk add build-base linux-headers`:

    cc -static -O2 -Wall -Wextra -ffile-prefix-map=$PWD=. -Wl,--build-id=none \
       -o pgoverlay-du-$(uname -m) pgoverlay-du.c && strip pgoverlay-du-$(uname -m)

Keep each binary small (a musl static build is well under 100 KiB): it travels
to the helper base64-encoded in environment variables, and a binary larger
than `cow.MaxDuToolSize` is ignored.

A binary whose SHA-256 does not match its `SHA256SUMS` line is ignored too.
When no usable binary exists for the helper's architecture, pgoverlay falls
back to `du -sb`, which counts reflinked extents as the branch's own: usage is
over-reported on XFS and btrfs hosts, never under-reported.
