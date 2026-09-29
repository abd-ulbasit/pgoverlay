# Reproducible builds of the lazyrw LD_PRELOAD shim (and of pgoverlay-du) for
# glibc and musl, per target platform. Driven by hack/build-lazyrw.sh with
# internal/cow as the build context; run `make lazyrw`, not this file directly.
#
# The same source must always give the same bytes: CI rebuilds and diffs
# against the committed dist/. So the base images are pinned by index digest,
# the toolchain packages by version, and the compiler is told to leave out
# everything host- or run-specific (build ID, ident string, source paths).
#
# glibc builds on Debian 11 (glibc 2.31) so the result loads on every newer
# glibc: postgres:14-18 are Debian 12 and 13. Debian 11 is past its LTS window,
# so its archive no longer changes. musl builds on Alpine 3.21 (musl 1.2.5).
#
# Bumping a pin changes the output: rebuild with `make lazyrw` and commit dist/.

ARG DEBIAN_IMAGE=debian:bullseye-slim@sha256:e5b6442dd2e9684cf5e87d8338b5968f3b348636fc0be6d7850a381e3731a2bd
ARG ALPINE_IMAGE=alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507

# ---- toolchains -------------------------------------------------------------

FROM ${DEBIAN_IMAGE} AS glibc-toolchain
# Packages come from the snapshot.debian.org timestamp the image itself was
# built from (commented out in its sources.list), so the pinned versions are
# there for good; deb.debian.org and archive.debian.org are the fallbacks.
RUN set -eu; \
    pkgs="gcc=4:10.2.1-1 gcc-10=10.2.1-6 libc6-dev=2.31-13+deb11u14 linux-libc-dev=5.10.262-1 binutils=2.35.2-2"; \
    install() { \
      apt-get -o Acquire::Retries=3 -o Acquire::Check-Valid-Until=false update -qq && \
      DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=3 install -y -qq --no-install-recommends $pkgs; \
    }; \
    cp /etc/apt/sources.list /tmp/sources.default; \
    sed -n 's|^# deb http://snapshot|deb http://snapshot|p' /tmp/sources.default > /etc/apt/sources.list; \
    if ! install; then \
      cp /tmp/sources.default /etc/apt/sources.list; \
      install || { sed 's|deb\.debian\.org|archive.debian.org|' /tmp/sources.default > /etc/apt/sources.list; install; }; \
    fi; \
    rm -rf /var/lib/apt/lists/* /tmp/sources.default

FROM ${ALPINE_IMAGE} AS musl-toolchain
RUN apk add --no-cache gcc=14.2.0-r4 musl-dev=1.2.5-r11 binutils=2.43.1-r3 linux-headers=6.6-r1

# ---- the shim -----------------------------------------------------------------
# -fno-ident drops the compiler's version string, --build-id=none the hash of
# the inputs, -ffile-prefix-map the build directory. -U_FORTIFY_SOURCE: the
# shim defines open() and friends itself, which fortify's inline wrappers
# would clash with. -fno-delete-null-pointer-checks keeps the shim's NULL
# checks although libc declares those arguments nonnull.

FROM glibc-toolchain AS glibc
ARG TARGETARCH
COPY lazyrw/lazyrw.c /src/lazyrw.c
RUN set -eu; \
    case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) echo "unsupported $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir -p /out; cd /src; \
    gcc -std=gnu11 -O2 -fPIC -Wall -Wextra -Werror -fvisibility=hidden -fno-delete-null-pointer-checks \
        -fstack-protector-strong -U_FORTIFY_SOURCE -fno-ident -ffile-prefix-map=/src=. \
        -shared -Wl,--build-id=none -Wl,--hash-style=gnu -Wl,-z,relro -Wl,-z,now -Wl,-z,noexecstack \
        -Wl,--no-undefined -o "/out/liblazyrw-glibc-$arch.so" lazyrw.c -ldl -lpthread; \
    strip --strip-all "/out/liblazyrw-glibc-$arch.so"

FROM musl-toolchain AS musl
ARG TARGETARCH
COPY lazyrw/lazyrw.c /src/lazyrw.c
RUN set -eu; \
    case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) echo "unsupported $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir -p /out; cd /src; \
    gcc -std=gnu11 -O2 -fPIC -Wall -Wextra -Werror -fvisibility=hidden -fno-delete-null-pointer-checks \
        -fstack-protector-strong -U_FORTIFY_SOURCE -fno-ident -ffile-prefix-map=/src=. \
        -shared -Wl,--build-id=none -Wl,--hash-style=gnu -Wl,-z,relro -Wl,-z,now -Wl,-z,noexecstack \
        -Wl,--no-undefined -o "/out/liblazyrw-musl-$arch.so" lazyrw.c; \
    strip --strip-all "/out/liblazyrw-musl-$arch.so"

# ---- pgoverlay-du (W4's FIEMAP exclusive-bytes counter): static, libc-free --------
# Built only once internal/cow/usage/pgoverlay-du.c exists. The context holds
# nothing but the C sources (Dockerfile.dockerignore), so copying all of it
# works whether or not that file is there yet.

FROM musl-toolchain AS du
ARG TARGETARCH
COPY . /src/
RUN set -eu; \
    case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; *) echo "unsupported $TARGETARCH" >&2; exit 1 ;; esac; \
    mkdir -p /out; cd /src/usage 2>/dev/null || exit 0; \
    if [ -f pgoverlay-du.c ]; then \
      gcc -std=gnu11 -O2 -Wall -Wextra -fstack-protector-strong -U_FORTIFY_SOURCE -fno-ident \
          -ffile-prefix-map=/src/usage=. -static -Wl,--build-id=none -o "/out/pgoverlay-du-$arch" pgoverlay-du.c; \
      strip --strip-all "/out/pgoverlay-du-$arch"; \
    fi

FROM scratch AS dist
COPY --from=glibc /out/ /
COPY --from=musl /out/ /
COPY --from=du /out/ /

# ---- C test binaries (not committed; hack/build-lazyrw.sh test) --------------

FROM glibc-toolchain AS glibc-test
ARG TARGETARCH
COPY lazyrw/test/lazyrw_test.c /src/lazyrw_test.c
RUN set -eu; \
    case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; esac; \
    mkdir -p /out; \
    gcc -std=gnu11 -O1 -Wall -Wextra -Werror -fno-builtin -U_FORTIFY_SOURCE -o "/out/lazyrw_test-glibc-$arch" /src/lazyrw_test.c -ldl

FROM musl-toolchain AS musl-test
ARG TARGETARCH
COPY lazyrw/test/lazyrw_test.c /src/lazyrw_test.c
RUN set -eu; \
    case "$TARGETARCH" in amd64) arch=x86_64 ;; arm64) arch=aarch64 ;; esac; \
    mkdir -p /out; \
    gcc -std=gnu11 -O1 -Wall -Wextra -Werror -fno-builtin -U_FORTIFY_SOURCE -o "/out/lazyrw_test-musl-$arch" /src/lazyrw_test.c

FROM scratch AS tests
COPY --from=glibc-test /out/ /
COPY --from=musl-test /out/ /
