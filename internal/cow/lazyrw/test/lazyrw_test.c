/*
 * lazyrw_test.c - C-level tests for the lazyrw shim on a real OverlayFS mount.
 *
 * run.sh assembles the overlay, makes this binary's working directory the
 * merged directory (as Postgres does with its data directory), sets PGDATA and
 * LD_PRELOAD, and runs it under the name "postgres" (the shim is active only
 * in a process with that name) or "notpostgres". Whether a lower file was
 * copied up is read straight from the upper directory.
 *
 * Usage:
 *   postgres mkfiles <dir>          write the lower-layer fixture files
 *   postgres active <upperdir>      the shim's behaviour (layout-independent)
 *   postgres enospc <upperdir>      upgrade failure and truncate-to-zero with a
 *                                   24 MiB upper and 32 MiB lower files
 *   postgres debug <upperdir>       one downgrade and one upgrade (run.sh checks
 *                                   the PGOVERLAY_LAZYRW_DEBUG lines)
 *   postgres noshim <upperdir>      control without the shim: copy on open
 *   notpostgres inert <upperdir>    H4: the shim stays inert
 *   postgres nopgdata <upperdir>    H4: inert without PGDATA
 *   postgres maxfd <upperdir>       H3: fds past the table are not downgraded
 *
 * Output is TAP-like; the exit status is the number of failed checks.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/sendfile.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <sys/wait.h>
#include <unistd.h>

#ifndef SYS_close_range
#define SYS_close_range 436
#endif

#define SMALL (256 * 1024)
#define BIG (32 * 1024 * 1024)
#define MAXFD 65536 /* keep in step with lazyrw.c */

static const char *g_upper;
static int g_failed, g_n;

static void check(int ok, const char *name, const char *fmt, ...) {
    g_n++;
    if (ok) {
        printf("ok %d - %s\n", g_n, name);
    } else {
        char buf[512] = "";
        if (fmt) {
            va_list ap;
            va_start(ap, fmt);
            vsnprintf(buf, sizeof buf, fmt, ap);
            va_end(ap);
        }
        printf("not ok %d - %s%s%s\n", g_n, name, *buf ? ": " : "", buf);
        g_failed++;
    }
    fflush(stdout);
}
#define CHECK(cond, name, ...) check(!!(cond), name, "" __VA_ARGS__)

/* ----- the shim's test hooks, looked up at run time ------------------------ */

static int state(int fd) {
    static int (*f)(int);
    if (!f) f = (int (*)(int))dlsym(RTLD_DEFAULT, "pgoverlay_lazyrw_state");
    return f ? f(fd) : -2;
}
static int shim_active(void) {
    int (*f)(void) = (int (*)(void))dlsym(RTLD_DEFAULT, "pgoverlay_lazyrw_active");
    return f ? f() : -2;
}
/* The *64, fortify and newer entry points, resolved the way a dynamically
 * linked caller binds them (the preloaded shim first). */
static void *sym(const char *name) {
    void *p = dlsym(RTLD_DEFAULT, name);
    if (!p) {
        fprintf(stderr, "missing symbol %s\n", name);
        exit(99);
    }
    return p;
}

/* ----- fixtures -------------------------------------------------------------- */

static unsigned char pattern(const char *name, size_t i) {
    unsigned h = 2166136261u;
    for (const char *c = name; *c; c++) h = (h ^ (unsigned char)*c) * 16777619u;
    return (unsigned char)((i * 31u + h) & 0xff);
}

static int write_fixture(const char *dir, const char *rel, size_t size) {
    char path[PATH_MAX];
    snprintf(path, sizeof path, "%s/%s", dir, rel);
    char *slash = strrchr(path, '/');
    *slash = 0;
    char cmd[PATH_MAX + 16];
    snprintf(cmd, sizeof cmd, "mkdir -p '%s'", path);
    if (system(cmd) != 0) return -1;
    *slash = '/';
    int fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0600);
    if (fd < 0) return -1;
    const char *base = strrchr(rel, '/') ? strrchr(rel, '/') + 1 : rel;
    unsigned char buf[65536];
    for (size_t off = 0; off < size; off += sizeof buf) {
        size_t n = size - off < sizeof buf ? size - off : sizeof buf;
        for (size_t i = 0; i < n; i++) buf[i] = pattern(base, off + i);
        if (write(fd, buf, n) != (ssize_t)n) return -1;
    }
    return close(fd);
}

static const char *const k_small[] = {
    "base/1/read", "base/1/write", "base/1/nocloexec", "base/1/pwrite", "base/1/pwrite64",
    "base/1/writev", "base/1/pwritev", "base/1/pwritev64", "base/1/pwritev2", "base/1/ftruncate",
    "base/1/fallocate", "base/1/posix_fallocate", "base/1/copy_file_range", "base/1/sendfile",
    "base/1/splice", "base/1/mmap", "base/1/mmapro", "base/1/trunc", "base/1/dup", "base/1/dupover",
    "base/1/stale", "base/1/stale2", "base/1/staleA", "base/1/staleB", "base/1/staleC",
    "base/1/closerange", "base/1/closefrom", "base/1/abs", "base/1/stack", "base/1/fork",
    "base/1/fortify", "base/1/openat", "base/1/open64", "base/1/wronly", "base/1/append",
    "base/1/creat", "base/1/otrunc", "base/1/rdonly", "base/1/dirfd", "base/1/maxfd",
    "base/1/inert", "base/1/nopgdata", "base/1/noshim", "base/1/enospc", "base/1/fallocate64",
    "base/1/ftruncate64", "base/1/sendfile64", "base/1/debug", "global/1262", "pg_xact/0000",
    "pg_wal/000000010000000000000001", "postgresql.conf", NULL,
};
static const char *const k_big[] = { "base/1/big", "base/1/big2", "base/1/big3", "base/1/big4", NULL };

static int mkfiles(const char *dir) {
    for (int i = 0; k_small[i]; i++)
        if (write_fixture(dir, k_small[i], SMALL) != 0) return perror(k_small[i]), 1;
    for (int i = 0; k_big[i]; i++)
        if (write_fixture(dir, k_big[i], BIG) != 0) return perror(k_big[i]), 1;
    return 0;
}

/* ----- helpers ----------------------------------------------------------------- */

/* Copied up: the file exists in the upper layer (not as a whiteout). */
static int in_upper(const char *rel) {
    char p[PATH_MAX];
    struct stat st;
    snprintf(p, sizeof p, "%s/%s", g_upper, rel);
    return stat(p, &st) == 0 && S_ISREG(st.st_mode);
}
static long long upper_size(const char *rel) {
    char p[PATH_MAX];
    struct stat st;
    snprintf(p, sizeof p, "%s/%s", g_upper, rel);
    return stat(p, &st) == 0 ? (long long)st.st_size : -1;
}

/* Does [off, off+n) of fd hold the fixture pattern for rel? */
static int matches_pattern(int fd, const char *rel, off_t off, size_t n) {
    const char *base = strrchr(rel, '/') ? strrchr(rel, '/') + 1 : rel;
    unsigned char *buf = malloc(n);
    if (!buf || pread(fd, buf, n, off) != (ssize_t)n) return free(buf), 0;
    for (size_t i = 0; i < n; i++)
        if (buf[i] != pattern(base, (size_t)off + i)) return free(buf), 0;
    free(buf);
    return 1;
}
static int has_bytes(int fd, off_t off, const char *want) {
    char buf[256];
    size_t n = strlen(want);
    return pread(fd, buf, n, off) == (ssize_t)n && memcmp(buf, want, n) == 0;
}
/* Read rel through a fresh read-only fd. */
static int file_has_bytes(const char *rel, off_t off, const char *want) {
    int fd = open(rel, O_RDONLY);
    if (fd < 0) return 0;
    int ok = has_bytes(fd, off, want);
    close(fd);
    return ok;
}
static int cloexec(int fd) { return (fcntl(fd, F_GETFD) & FD_CLOEXEC) != 0; }
static int accmode(int fd) { return fcntl(fd, F_GETFL) & O_ACCMODE; }

/* Open rel O_RDWR and require that the shim downgraded it. */
static int open_downgraded(const char *rel, int extra, const char *name) {
    int fd = open(rel, O_RDWR | extra);
    char label[256];
    snprintf(label, sizeof label, "%s: O_RDWR open is downgraded, nothing copied", name);
    CHECK(fd >= 0 && state(fd) == 1 && accmode(fd) == O_RDONLY && !in_upper(rel), label,
          "fd=%d state=%d in_upper=%d errno=%d", fd, fd >= 0 ? state(fd) : -9, in_upper(rel), errno);
    return fd;
}

/* After a write-class call: upgraded, copied up, the new bytes are there and
 * the rest of the file is intact. */
static void expect_upgraded(int fd, const char *rel, off_t off, const char *want, const char *name) {
    char label[256];
    snprintf(label, sizeof label, "%s: upgrades on first write and keeps the data", name);
    int ok = state(fd) == 2 && accmode(fd) != O_RDONLY && in_upper(rel) && has_bytes(fd, off, want) &&
             matches_pattern(fd, rel, 0, (size_t)off) && file_has_bytes(rel, off, want);
    CHECK(ok, label, "state=%d accmode=%d in_upper=%d", state(fd), accmode(fd), in_upper(rel));
}

/* ----- the active suite ----------------------------------------------------------- */

static void t_read_no_copy(void) {
    const char *rel = "base/1/read";
    int fd = open_downgraded(rel, 0, "read");
    CHECK(matches_pattern(fd, rel, 0, SMALL), "read: data reads back through the downgraded fd");
    CHECK(fsync(fd) == 0 && fdatasync(fd) == 0, "read: fsync and fdatasync work on the downgraded fd");
    CHECK(sync_file_range(fd, 0, 0, SYNC_FILE_RANGE_WRITE) == 0, "read: sync_file_range needs no upgrade");
    CHECK(posix_fadvise(fd, 0, 0, POSIX_FADV_DONTNEED) == 0, "read: posix_fadvise needs no upgrade");
    struct stat st;
    CHECK(fstat(fd, &st) == 0 && st.st_size == SMALL, "read: fstat reports the lower file");
    CHECK(state(fd) == 1 && !in_upper(rel), "read: a reader never copies the file up");
    close(fd);
    CHECK(state(fd) == 0, "read: close forgets the fd");
    CHECK(!in_upper(rel), "read: still not copied after close");
}

static void t_write_upgrades(void) {
    const char *rel = "base/1/write";
    int fd = open_downgraded(rel, O_CLOEXEC, "write");
    char buf[10];
    CHECK(lseek(fd, 100, SEEK_SET) == 100 && read(fd, buf, 10) == 10, "write: positioned read before the upgrade");
    CHECK(write(fd, "HELLO", 5) == 5, "write: write() succeeds", "errno=%d", errno);
    CHECK(lseek(fd, 0, SEEK_CUR) == 115, "write: the file offset survives the upgrade (110 + 5)");
    CHECK(cloexec(fd), "write: FD_CLOEXEC survives the upgrade");
    CHECK(accmode(fd) == O_RDWR, "write: the fd is O_RDWR after the upgrade");
    expect_upgraded(fd, rel, 110, "HELLO", "write");
    CHECK(matches_pattern(fd, rel, 115, SMALL - 115), "write: the tail is intact");
    CHECK(write(fd, "!", 1) == 1 && state(fd) == 2 && has_bytes(fd, 115, "!"), "write: later writes go straight through");
    close(fd);
}

static void t_nocloexec(void) {
    const char *rel = "base/1/nocloexec";
    int fd = open_downgraded(rel, 0, "nocloexec");
    CHECK(pwrite(fd, "x", 1, 0) == 1 && state(fd) == 2, "nocloexec: pwrite upgrades");
    CHECK(!cloexec(fd), "nocloexec: no FD_CLOEXEC appears after the upgrade");
    close(fd);
}

static void t_write_class(void) {
    const char *rel;
    int fd;

    rel = "base/1/pwrite";
    fd = open_downgraded(rel, 0, "pwrite");
    CHECK(pwrite(fd, "PW", 2, 4096) == 2, "pwrite: returns 2", "errno=%d", errno);
    expect_upgraded(fd, rel, 4096, "PW", "pwrite");
    close(fd);

    rel = "base/1/pwrite64";
    fd = open_downgraded(rel, 0, "pwrite64");
    ssize_t (*pw64)(int, const void *, size_t, off_t) = sym("pwrite64");
    CHECK(pw64(fd, "P6", 2, 8192) == 2, "pwrite64: returns 2", "errno=%d", errno);
    expect_upgraded(fd, rel, 8192, "P6", "pwrite64");
    close(fd);

    struct iovec iov[2] = { { "AB", 2 }, { "CD", 2 } };
    rel = "base/1/writev";
    fd = open_downgraded(rel, 0, "writev");
    lseek(fd, 50, SEEK_SET);
    CHECK(writev(fd, iov, 2) == 4, "writev: returns 4", "errno=%d", errno);
    expect_upgraded(fd, rel, 50, "ABCD", "writev");
    close(fd);

    rel = "base/1/pwritev";
    fd = open_downgraded(rel, 0, "pwritev");
    CHECK(pwritev(fd, iov, 2, 60) == 4, "pwritev: returns 4", "errno=%d", errno);
    expect_upgraded(fd, rel, 60, "ABCD", "pwritev");
    close(fd);

    rel = "base/1/pwritev64";
    fd = open_downgraded(rel, 0, "pwritev64");
    ssize_t (*pv64)(int, const struct iovec *, int, off_t) = sym("pwritev64");
    CHECK(pv64(fd, iov, 2, 70) == 4, "pwritev64: returns 4", "errno=%d", errno);
    expect_upgraded(fd, rel, 70, "ABCD", "pwritev64");
    close(fd);

    rel = "base/1/pwritev2";
    fd = open_downgraded(rel, 0, "pwritev2");
    ssize_t (*pv2)(int, const struct iovec *, int, off_t, int) = sym("pwritev2");
    CHECK(pv2(fd, iov, 2, 80, 0) == 4, "pwritev2: returns 4", "errno=%d", errno);
    expect_upgraded(fd, rel, 80, "ABCD", "pwritev2");
    close(fd);

    rel = "base/1/ftruncate";
    fd = open_downgraded(rel, 0, "ftruncate");
    CHECK(ftruncate(fd, 1000) == 0, "ftruncate(len>0): returns 0", "errno=%d", errno);
    struct stat st;
    CHECK(fstat(fd, &st) == 0 && st.st_size == 1000 && state(fd) == 2 && in_upper(rel) &&
              matches_pattern(fd, rel, 0, 1000),
          "ftruncate(len>0): upgrades, shortens, keeps the head");
    close(fd);

    rel = "base/1/ftruncate64";
    fd = open_downgraded(rel, 0, "ftruncate64");
    int (*ft64)(int, off_t) = sym("ftruncate64");
    CHECK(ft64(fd, 2000) == 0 && fstat(fd, &st) == 0 && st.st_size == 2000 && state(fd) == 2 &&
              matches_pattern(fd, rel, 0, 2000),
          "ftruncate64(len>0): upgrades, shortens, keeps the head");
    close(fd);

    rel = "base/1/fallocate";
    fd = open_downgraded(rel, 0, "fallocate");
    int r = fallocate(fd, 0, 0, SMALL + 8192);
    if (r != 0 && errno == EOPNOTSUPP) r = 0; /* the upgrade is what is tested */
    CHECK(r == 0 && state(fd) == 2 && in_upper(rel) && matches_pattern(fd, rel, 0, SMALL),
          "fallocate: upgrades and keeps the data", "errno=%d", errno);
    close(fd);

    rel = "base/1/fallocate64";
    fd = open_downgraded(rel, 0, "fallocate64");
    int (*fa64)(int, int, off_t, off_t) = sym("fallocate64");
    r = fa64(fd, 0, 0, SMALL + 8192);
    if (r != 0 && errno == EOPNOTSUPP) r = 0;
    CHECK(r == 0 && state(fd) == 2 && in_upper(rel), "fallocate64: upgrades", "errno=%d", errno);
    close(fd);

    rel = "base/1/posix_fallocate";
    fd = open_downgraded(rel, 0, "posix_fallocate");
    CHECK(posix_fallocate(fd, 0, SMALL + 16384) == 0 && fstat(fd, &st) == 0 && st.st_size == SMALL + 16384 &&
              state(fd) == 2 && matches_pattern(fd, rel, 0, SMALL),
          "posix_fallocate: upgrades, extends and keeps the data");
    close(fd);

    /* The source is a file already copied up, so that source and destination
     * share a real filesystem in either layout (OverlayFS refuses a copy
     * between two with EXDEV). */
    rel = "base/1/copy_file_range";
    fd = open_downgraded(rel, 0, "copy_file_range");
    int cfr_src = open("base/1/write", O_RDONLY);
    off_t in_off = 0, out_off = 300;
    ssize_t cr = copy_file_range(cfr_src, &in_off, fd, &out_off, 16, 0);
    char cfr_want[16];
    for (int i = 0; i < 16; i++) cfr_want[i] = (char)pattern("write", (size_t)i);
    CHECK(cr == 16, "copy_file_range: copies into the downgraded destination", "ret=%zd errno=%d", cr, errno);
    CHECK(state(fd) == 2 && in_upper(rel), "copy_file_range: the destination upgraded");
    {
        char got[16];
        CHECK(pread(fd, got, 16, 300) == 16 && memcmp(got, cfr_want, 16) == 0 && matches_pattern(fd, rel, 0, 300),
              "copy_file_range: the bytes landed and the head is intact");
    }
    close(cfr_src);
    close(fd);

    int src = open("postgresql.conf", O_RDONLY);
    char want[16];
    for (int i = 0; i < 16; i++) want[i] = (char)pattern("postgresql.conf", (size_t)i);

    rel = "base/1/sendfile";
    fd = open_downgraded(rel, 0, "sendfile");
    off_t soff = 0;
    lseek(fd, 400, SEEK_SET);
    ssize_t sf = sendfile(fd, src, &soff, 16);
    char got[16];
    CHECK(sf == 16 && state(fd) == 2 && pread(fd, got, 16, 400) == 16 && memcmp(got, want, 16) == 0,
          "sendfile: upgrades the output fd and the bytes land", "ret=%zd errno=%d", sf, errno);
    close(fd);

    rel = "base/1/sendfile64";
    fd = open_downgraded(rel, 0, "sendfile64");
    ssize_t (*sf64)(int, int, off_t *, size_t) = sym("sendfile64");
    soff = 0;
    lseek(fd, 400, SEEK_SET);
    sf = sf64(fd, src, &soff, 16);
    CHECK(sf == 16 && state(fd) == 2 && pread(fd, got, 16, 400) == 16 && memcmp(got, want, 16) == 0,
          "sendfile64: upgrades the output fd", "ret=%zd errno=%d", sf, errno);
    close(fd);
    close(src);

    rel = "base/1/splice";
    fd = open_downgraded(rel, 0, "splice");
    int p[2];
    CHECK(pipe(p) == 0 && write(p[1], "SPLICED", 7) == 7, "splice: pipe ready");
    off_t spoff = 500;
    ssize_t sp = splice(p[0], NULL, fd, &spoff, 7, 0);
    CHECK(sp == 7, "splice: returns 7", "ret=%zd errno=%d", sp, errno);
    expect_upgraded(fd, rel, 500, "SPLICED", "splice");
    close(p[0]);
    close(p[1]);
    close(fd);

    rel = "base/1/mmap";
    fd = open_downgraded(rel, 0, "mmap");
    char *m = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    CHECK(m != MAP_FAILED, "mmap(PROT_WRITE, MAP_SHARED): maps after an upgrade", "errno=%d", errno);
    if (m != MAP_FAILED) {
        memcpy(m + 600, "MAPPED", 6);
        msync(m, 4096, MS_SYNC);
        munmap(m, 4096);
    }
    expect_upgraded(fd, rel, 600, "MAPPED", "mmap");
    close(fd);

    rel = "base/1/mmapro";
    fd = open_downgraded(rel, 0, "mmapro");
    void *m1 = mmap(NULL, 4096, PROT_READ, MAP_SHARED, fd, 0);
    void *m2 = mmap(NULL, 4096, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
    CHECK(m1 != MAP_FAILED && m2 != MAP_FAILED && state(fd) == 1 && !in_upper(rel),
          "mmap: read-only or private mappings need no upgrade");
    if (m1 != MAP_FAILED) munmap(m1, 4096);
    if (m2 != MAP_FAILED) munmap(m2, 4096);
    close(fd);
}

/* H1 in any layout: truncation to zero works and leaves an empty upper file. */
static void t_truncate(void) {
    const char *rel = "base/1/big";
    int fd = open_downgraded(rel, 0, "ftruncate(0)");
    CHECK(ftruncate(fd, 0) == 0, "ftruncate(0): returns 0", "errno=%d", errno);
    struct stat st;
    CHECK(fstat(fd, &st) == 0 && st.st_size == 0 && state(fd) == 2 && upper_size(rel) == 0,
          "ftruncate(0): upgraded with O_TRUNC, the upper file is empty", "size=%lld state=%d upper=%lld",
          (long long)st.st_size, state(fd), upper_size(rel));
    CHECK(pwrite(fd, "Z", 1, 0) == 1 && has_bytes(fd, 0, "Z"), "ftruncate(0): the fd stays writable");
    close(fd);

    rel = "base/1/big2";
    CHECK(truncate(rel, 0) == 0, "truncate(0): returns 0", "errno=%d", errno);
    CHECK(upper_size(rel) == 0 && stat(rel, &st) == 0 && st.st_size == 0, "truncate(0): the upper file is empty");

    rel = "base/1/trunc";
    CHECK(truncate(rel, 100) == 0 && stat(rel, &st) == 0 && st.st_size == 100, "truncate(len>0): passes through");
    CHECK(in_upper(rel) && upper_size(rel) == 100, "truncate(len>0): copied up and shortened");
    fd = open(rel, O_RDONLY);
    CHECK(matches_pattern(fd, rel, 0, 100), "truncate(len>0): the head is intact");
    close(fd);

    CHECK(truncate("base/1/missing", 0) == -1 && errno == ENOENT, "truncate(0): a missing file is ENOENT");
}

static void t_dup(void) {
    const char *rel = "base/1/dup";
    int fd = open_downgraded(rel, 0, "dup");
    int d1 = dup(fd);
    int d2 = dup2(fd, 100);
    int d3 = dup3(fd, 101, O_CLOEXEC);
    int d4 = fcntl(fd, F_DUPFD, 200);
    int d5 = fcntl(fd, F_DUPFD_CLOEXEC, 300);
    CHECK(d1 >= 0 && state(d1) == 1, "dup: tracks the new fd");
    CHECK(d2 == 100 && state(100) == 1, "dup2: tracks the new fd");
    CHECK(d3 == 101 && state(101) == 1 && cloexec(101), "dup3: tracks the new fd");
    CHECK(d4 >= 200 && state(d4) == 1, "F_DUPFD: tracks the new fd");
    CHECK(d5 >= 300 && state(d5) == 1 && cloexec(d5), "F_DUPFD_CLOEXEC: tracks the new fd");
    CHECK(dup2(fd, fd) == fd && state(fd) == 1, "dup2(fd, fd): keeps the tracking");
    CHECK(pwrite(101, "D3", 2, 0) == 2 && state(101) == 2 && cloexec(101) && state(fd) == 1 && state(d1) == 1,
          "dup3 fd: upgrades on its own and keeps FD_CLOEXEC");
    CHECK(pwrite(fd, "FD", 2, 2) == 2 && state(fd) == 2, "original fd: upgrades on its own write");
    CHECK(pwrite(d4, "D4", 2, 4) == 2 && pwrite(d5, "D5", 2, 6) == 2 && pwrite(d1, "D1", 2, 8) == 2 &&
              pwrite(100, "D2", 2, 10) == 2,
          "every duplicate writes");
    CHECK(file_has_bytes(rel, 0, "D3FDD4D5D1D2"), "all writes landed in the one upper file");
    CHECK(matches_pattern(d1, rel, 12, SMALL - 12), "dup: the rest of the file is intact");
    int ds[] = { fd, d1, 100, 101, d4, d5 };
    int all_zero = 1;
    for (int i = 0; i < 6; i++) {
        close(ds[i]);
        if (state(ds[i]) != 0) all_zero = 0;
    }
    CHECK(all_zero, "close: forgets every duplicate");

    fd = open_downgraded("base/1/dupover", 0, "dup2 over a tracked fd");
    int other = open("base/1/read", O_RDONLY);
    CHECK(dup2(other, fd) == fd && state(fd) == 0, "dup2 onto a tracked fd: the slot takes the source's state");
    close(other);
    close(fd);
}

/* H2: an fd closed behind the shim's back and reused must never be swapped. */
static void t_stale(void) {
    int a = open_downgraded("base/1/stale", 0, "stale pipe");
    int b = open_downgraded("base/1/stale2", 0, "stale pipe (2)");
    syscall(SYS_close, a); /* not through the shim: the slots go stale */
    syscall(SYS_close, b);
    int p[2];
    CHECK(pipe(p) == 0 && p[0] == a && p[1] == b && state(b) == 1, "stale: a pipe reuses the stale numbers",
          "p=%d,%d a=%d b=%d", p[0], p[1], a, b);
    CHECK(write(p[1], "PIPE", 4) == 4, "stale: a write to the reused fd goes to the pipe", "errno=%d", errno);
    char buf[4];
    CHECK(read(p[0], buf, 4) == 4 && memcmp(buf, "PIPE", 4) == 0, "stale: the pipe got the bytes");
    CHECK(state(b) == 0 && !in_upper("base/1/stale2") && !in_upper("base/1/stale"),
          "stale: the slot was forgotten and no data file was touched");
    int s2 = open("base/1/stale2", O_RDONLY);
    CHECK(matches_pattern(s2, "base/1/stale2", 0, SMALL), "stale: the data file is unchanged");
    close(s2);
    close(p[0]);
    close(p[1]);

    int s = open_downgraded("base/1/stale", 0, "stale socket");
    syscall(SYS_close, s);
    int sv[2];
    CHECK(socketpair(AF_UNIX, SOCK_STREAM, 0, sv) == 0 && sv[0] == s, "stale: a socket reuses the stale number");
    CHECK(write(sv[0], "SOCK", 4) == 4 && read(sv[1], buf, 4) == 4 && memcmp(buf, "SOCK", 4) == 0,
          "stale: a write to the reused fd goes to the socket");
    CHECK(state(s) == 0 && !in_upper("base/1/stale"), "stale: socket slot forgotten, data file untouched");
    close(sv[0]);
    close(sv[1]);

    /* Reused by a different regular file opened read-only: the write fails
     * with EBADF exactly as it would without the shim. */
    int fa = open_downgraded("base/1/staleA", 0, "stale other file");
    syscall(SYS_close, fa);
    int fb = (int)syscall(SYS_openat, AT_FDCWD, "base/1/staleB", O_RDONLY, 0);
    CHECK(fb == fa && state(fb) == 1, "stale: another file reuses the number");
    errno = 0;
    CHECK(pwrite(fb, "X", 1, 0) == -1 && errno == EBADF, "stale: write to a read-only reuse is EBADF", "errno=%d",
          errno);
    CHECK(state(fb) == 0 && !in_upper("base/1/staleA") && !in_upper("base/1/staleB"),
          "stale: neither file was copied up");
    close(fb);

    /* Reused by a different file opened read-write: the write goes there. */
    fa = open_downgraded("base/1/staleA", 0, "stale rw file");
    syscall(SYS_close, fa);
    int fc = (int)syscall(SYS_openat, AT_FDCWD, "base/1/staleC", O_RDWR, 0);
    CHECK(fc == fa && pwrite(fc, "C!", 2, 0) == 2 && state(fc) == 0, "stale: a read-write reuse writes normally");
    CHECK(file_has_bytes("base/1/staleC", 0, "C!") && !in_upper("base/1/staleA"),
          "stale: the bytes went to the new file only");
    close(fc);
}

static void t_close_range(void) {
    int (*cr)(unsigned int, unsigned int, int) = sym("close_range");
    void (*cf)(int) = sym("closefrom");

    int fd = open_downgraded("base/1/closerange", 0, "close_range");
    CHECK(cr((unsigned)fd, (unsigned)fd, 4 /* CLOSE_RANGE_CLOEXEC */) == 0 && state(fd) == 1 && cloexec(fd),
          "close_range(CLOEXEC): keeps the tracking");
    CHECK(cr((unsigned)fd, (unsigned)fd, 0) == 0 && state(fd) == 0 && fcntl(fd, F_GETFD) == -1,
          "close_range: closes and forgets");

    fd = open_downgraded("base/1/closefrom", 0, "closefrom");
    CHECK(dup2(fd, 700) == 700 && state(700) == 1, "closefrom: tracked fd at 700");
    close(fd);
    cf(700);
    CHECK(state(700) == 0 && fcntl(700, F_GETFD) == -1, "closefrom: closes and forgets");
}

static void t_not_downgraded(void) {
    int fd = open("pg_wal/000000010000000000000001", O_RDWR);
    CHECK(fd >= 0 && state(fd) == 0 && in_upper("pg_wal/000000010000000000000001"), "pg_wal: never downgraded");
    close(fd);
    fd = open("postgresql.conf", O_RDWR);
    CHECK(fd >= 0 && state(fd) == 0 && in_upper("postgresql.conf"), "non-data path: never downgraded");
    close(fd);
    fd = open("base/1/creat", O_RDWR | O_CREAT, 0600);
    CHECK(fd >= 0 && state(fd) == 0 && in_upper("base/1/creat"), "O_CREAT: never downgraded");
    close(fd);
    fd = open("base/1/otrunc", O_RDWR | O_TRUNC);
    CHECK(fd >= 0 && state(fd) == 0 && upper_size("base/1/otrunc") == 0, "O_TRUNC: never downgraded (and truncates)");
    close(fd);
    fd = open("base/1/rdonly", O_RDONLY);
    CHECK(fd >= 0 && state(fd) == 0 && !in_upper("base/1/rdonly"), "O_RDONLY: untouched");
    close(fd);
    int dfd = open("base", O_RDONLY | O_DIRECTORY);
    fd = openat(dfd, "1/dirfd", O_RDWR);
    CHECK(fd >= 0 && state(fd) == 0 && in_upper("base/1/dirfd"), "openat with a real dirfd: passed through");
    close(fd);
    close(dfd);

    int g = open("global/1262", O_RDWR);
    int x = open("pg_xact/0000", O_RDWR);
    CHECK(g >= 0 && x >= 0 && state(g) == 1 && state(x) == 1, "global/ and pg_xact/: downgraded");
    close(g);
    close(x);
}

static void t_open_variants(void) {
    char abs[PATH_MAX];
    snprintf(abs, sizeof abs, "%s/base/1/abs", getenv("PGDATA"));
    int fd = open(abs, O_RDWR);
    CHECK(fd >= 0 && state(fd) == 1 && !in_upper("base/1/abs"), "absolute path under PGDATA: downgraded");
    CHECK(pwrite(fd, "ABS", 3, 0) == 3, "absolute path: write");
    expect_upgraded(fd, "base/1/abs", 0, "ABS", "absolute path");
    close(fd);

    int (*o2)(const char *, int) = sym("__open_2");
    int (*oa2)(int, const char *, int) = sym("__openat_2");
    int (*o64)(const char *, int, ...) = sym("open64");
    int (*oa64)(int, const char *, int, ...) = sym("openat64");
    int f1 = o2("base/1/fortify", O_RDWR);
    int f2 = oa2(AT_FDCWD, "base/1/fortify", O_RDWR);
    int f3 = o64("base/1/open64", O_RDWR);
    int f4 = oa64(AT_FDCWD, "base/1/open64", O_RDWR);
    int f5 = openat(AT_FDCWD, "base/1/openat", O_RDWR);
    CHECK(state(f1) == 1 && state(f2) == 1 && state(f3) == 1 && state(f4) == 1 && state(f5) == 1,
          "__open_2, __openat_2, open64, openat64, openat(AT_FDCWD): downgraded");
    CHECK(!in_upper("base/1/fortify") && !in_upper("base/1/open64") && !in_upper("base/1/openat"),
          "open variants: nothing copied");
    close(f1);
    close(f2);
    close(f3);
    close(f4);
    close(f5);

    fd = open("base/1/wronly", O_WRONLY);
    CHECK(fd >= 0 && state(fd) == 1 && accmode(fd) == O_RDONLY, "O_WRONLY: downgraded");
    CHECK(write(fd, "WO", 2) == 2 && state(fd) == 2 && accmode(fd) == O_WRONLY && file_has_bytes("base/1/wronly", 0, "WO"),
          "O_WRONLY: upgrades back to O_WRONLY");
    close(fd);

    fd = open("base/1/append", O_RDWR | O_APPEND);
    CHECK(fd >= 0 && state(fd) == 1, "O_APPEND: downgraded");
    CHECK(write(fd, "TAIL", 4) == 4 && (fcntl(fd, F_GETFL) & O_APPEND) && file_has_bytes("base/1/append", SMALL, "TAIL"),
          "O_APPEND: the upgraded fd still appends");
    close(fd);
}

/* The kernel property the shim depends on (stacked file operations, 4.19+):
 * a read-only fd opened before a copy-up reads the upper file after it. */
static void t_stacked(void) {
    const char *rel = "base/1/stack";
    int a = open_downgraded(rel, 0, "stacked reader");
    CHECK(matches_pattern(a, rel, 0, 64), "stacked: reader sees the lower data");
    int b = open_downgraded(rel, 0, "stacked writer");
    CHECK(pwrite(b, "NEWDATA", 7, 0) == 7 && state(b) == 2, "stacked: writer upgrades");
    CHECK(has_bytes(a, 0, "NEWDATA"), "stacked: the older read-only fd sees the write");
    CHECK(pwrite(a, "OLDFD", 5, 16) == 5 && state(a) == 2 && file_has_bytes(rel, 16, "OLDFD"),
          "stacked: the older fd upgrades too after another fd's copy-up");
    CHECK(has_bytes(b, 0, "NEWDATA") && has_bytes(b, 16, "OLDFD"), "stacked: both writes are in one file");
    close(a);
    close(b);
}

static void t_fork(void) {
    const char *rel = "base/1/fork";
    int fd = open_downgraded(rel, 0, "fork");
    pid_t pid = fork();
    if (pid == 0) {
        int bad = 0;
        if (state(fd) != 1) bad |= 1;
        if (pwrite(fd, "CHILD", 5, 0) != 5) bad |= 2;
        if (state(fd) != 2) bad |= 4;
        _exit(bad);
    }
    int status = -1;
    waitpid(pid, &status, 0);
    CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0, "fork: the child inherits the tracking and upgrades",
          "status=%d", status);
    CHECK(state(fd) == 1, "fork: the parent's fd is still downgraded");
    CHECK(has_bytes(fd, 0, "CHILD"), "fork: the parent's read-only fd sees the child's write");
    CHECK(pwrite(fd, "PARENT", 6, 8) == 6 && state(fd) == 2 && file_has_bytes(rel, 0, "CHILD") &&
              file_has_bytes(rel, 8, "PARENT"),
          "fork: the parent upgrades on its own write");
    close(fd);
}

static int suite_active(void) {
    CHECK(shim_active() == 1, "the shim is active in a process named postgres with PGDATA set",
          "active=%d", shim_active());
    t_read_no_copy();
    t_write_upgrades();
    t_nocloexec();
    t_write_class();
    t_truncate();
    t_dup();
    t_stale();
    t_close_range();
    t_not_downgraded();
    t_open_variants();
    t_stacked();
    t_fork();
    return g_failed;
}

/* ----- other modes ------------------------------------------------------------- */

/* Upper layer of 24 MiB, lower files of 32 MiB: a whole-file copy-up cannot
 * fit, so success proves nothing was copied and failure must be loud. */
static int suite_enospc(void) {
    const char *rel = "base/1/big3";
    int fd = open_downgraded(rel, 0, "enospc");
    errno = 0;
    CHECK(pwrite(fd, "X", 1, 0) == -1 && errno == ENOSPC, "enospc: a failed upgrade fails the write with its errno",
          "errno=%d", errno);
    CHECK(state(fd) == 1, "enospc: the fd stays downgraded");
    CHECK(posix_fallocate(fd, 0, 1) == ENOSPC, "enospc: posix_fallocate returns the errno");
    CHECK(matches_pattern(fd, rel, 0, 4096), "enospc: the data still reads back");
    close(fd);

    fd = open_downgraded("base/1/big", 0, "enospc ftruncate(0)");
    CHECK(ftruncate(fd, 0) == 0 && upper_size("base/1/big") == 0,
          "enospc: ftruncate(0) of a 32 MiB lower file copies nothing", "errno=%d", errno);
    close(fd);
    CHECK(truncate("base/1/big2", 0) == 0 && upper_size("base/1/big2") == 0,
          "enospc: truncate(0) of a 32 MiB lower file copies nothing", "errno=%d", errno);
    return g_failed;
}

/* Without the shim the environment really copies on open (so the checks
 * above can see a copy), and truncate(0) of a big lower file needs the space. */
static int suite_noshim(void) {
    CHECK(shim_active() == -2, "noshim: no shim loaded");
    int fd = open("base/1/noshim", O_RDWR);
    CHECK(fd >= 0 && in_upper("base/1/noshim"), "noshim: an O_RDWR open copies the file up");
    close(fd);
    errno = 0;
    CHECK(truncate("base/1/big4", 0) == -1 && errno == ENOSPC,
          "noshim: truncate(0) of a 32 MiB lower file copies it first (ENOSPC here)", "errno=%d", errno);
    return g_failed;
}

static int suite_inert(const char *rel, const char *what) {
    char label[128];
    snprintf(label, sizeof label, "%s: the shim is loaded but inert", what);
    CHECK(shim_active() == 0, label, "active=%d", shim_active());
    int fd = open(rel, O_RDWR);
    snprintf(label, sizeof label, "%s: O_RDWR opens are passed through", what);
    CHECK(fd >= 0 && state(fd) == 0 && in_upper(rel), label);
    snprintf(label, sizeof label, "%s: writes work", what);
    CHECK(pwrite(fd, "I", 1, 0) == 1, label);
    close(fd);
    return g_failed;
}

static int suite_maxfd(void) {
    struct rlimit rl = { MAXFD + 64, MAXFD + 64 };
    if (setrlimit(RLIMIT_NOFILE, &rl) != 0) {
        printf("ok 1 - # SKIP maxfd: cannot raise RLIMIT_NOFILE (errno %d)\n", errno);
        return 0;
    }
    int devnull = open("/dev/null", O_RDONLY);
    for (int i = 3; i < MAXFD; i++)
        if (fcntl(i, F_GETFD) == -1) syscall(SYS_dup3, devnull, i, 0);
    int fd = open("base/1/maxfd", O_RDWR);
    CHECK(fd >= MAXFD, "maxfd: the next fd is past the table", "fd=%d", fd);
    CHECK(state(fd) == -1 && accmode(fd) == O_RDWR && in_upper("base/1/maxfd"),
          "maxfd: opened the way the caller asked (eager copy-up)");
    CHECK(pwrite(fd, "HI", 2, 0) == 2 && file_has_bytes("base/1/maxfd", 0, "HI"), "maxfd: writes work");
    close(fd);
    syscall(SYS_close_range, 3, MAXFD - 1, 0);
    return g_failed;
}

int main(int argc, char **argv) {
    if (argc < 3) {
        fprintf(stderr, "usage: %s mode dir\n", argv[0]);
        return 2;
    }
    const char *mode = argv[1];
    if (strcmp(mode, "mkfiles") == 0) return mkfiles(argv[2]);
    g_upper = argv[2];
    if (strcmp(mode, "active") == 0) return suite_active();
    if (strcmp(mode, "enospc") == 0) return suite_enospc();
    if (strcmp(mode, "noshim") == 0) return suite_noshim();
    if (strcmp(mode, "inert") == 0) return suite_inert("base/1/inert", "inert (not postgres)");
    if (strcmp(mode, "nopgdata") == 0) return suite_inert("base/1/nopgdata", "inert (no PGDATA)");
    if (strcmp(mode, "maxfd") == 0) return suite_maxfd();
    if (strcmp(mode, "debug") == 0) {
        int fd = open("base/1/debug", O_RDWR);
        int ok = fd >= 0 && pwrite(fd, "DBG", 3, 0) == 3 && state(fd) == 2;
        close(fd);
        return !ok;
    }
    fprintf(stderr, "unknown mode %s\n", mode);
    return 2;
}
