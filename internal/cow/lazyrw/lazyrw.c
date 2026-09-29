/*
 * lazyrw.c - LD_PRELOAD shim that makes stock PostgreSQL on OverlayFS copy a
 * relation file up on its first WRITE instead of on open.
 *
 * PostgreSQL opens every relation segment O_RDWR, even to read it (md.c). On
 * OverlayFS a read-write open of a lower-layer file copies the whole file into
 * the upper layer, so a read-only SELECT copies whole tables into a branch.
 *
 * The shim interposes the open family. A write-intent open (O_RDWR/O_WRONLY
 * without O_CREAT, O_TRUNC or O_TMPFILE) of a regular file in a relation or
 * SLRU directory under PGDATA is performed O_RDONLY instead, and the fd, the
 * path, the caller's flags and the file's st_dev/st_ino are remembered. On the
 * first write-class call on that fd the path is reopened with the original
 * flags (this is where OverlayFS copies the file up) and the new description is
 * dup3'd onto the same fd number, keeping FD_CLOEXEC, the file offset and the
 * status flags; then the call proceeds. A process that only reads never
 * upgrades, so the lower file is never copied.
 *
 * Correctness depends on OverlayFS re-targeting read-only fds opened before a
 * copy-up to the upper file (stacked file operations, Linux 4.19+): another
 * backend's read-only fd must see data written through the upgraded fd. The
 * branch entrypoint self-tests that property and falls back to eager mode.
 *
 * Hardening (pgoverlay issue #49):
 *  H1 truncate(path, 0) of a data file becomes open(O_WRONLY|O_TRUNC) + close,
 *     and ftruncate(fd, 0) of a downgraded fd upgrades with O_TRUNC: OverlayFS
 *     copies up zero bytes for an O_TRUNC open, but truncate() and ftruncate()
 *     of a lower file first copy all of it (DROP, TRUNCATE, VACUUM FULL,
 *     CLUSTER and table rewrites call these).
 *  H2 An upgrade swaps only an fd that still is the file it was: a regular
 *     file, open O_RDONLY, with the recorded st_dev/st_ino (or, if a copy-up by
 *     another process changed what fstat reports, the same file as the path
 *     now). Otherwise the slot is stale - the fd was closed by a call the shim
 *     does not see (glibc-internal close) and reused - so it is forgotten and
 *     the call passes through untouched.
 *  H3 An fd outside the tracking table is never downgraded: it is reopened
 *     with the caller's flags (eager copy-up, still correct).
 *  H4 Active only in the postgres server binary with PGDATA set, decided once
 *     in the constructor (before ps_status clobbers argv and moves environ).
 *     Everything else (entrypoint shells, gosu, archive_command, COPY PROGRAM)
 *     gets pure pass-through wrappers.
 *  H5 The whole write surface is interposed (write, pwrite*, writev, pwritev*,
 *     ftruncate*, fallocate*, posix_fallocate*, copy_file_range, sendfile*,
 *     splice, shared writable mmap*) plus fd bookkeeping (close, close_range,
 *     closefrom, dup*, fcntl* F_DUPFD*). Every call into libc goes through a
 *     pointer that may be NULL early in process start-up, so each has a raw
 *     syscall fallback. sync_file_range is not interposed: it is valid on a
 *     read-only fd.
 *  H6 A failed upgrade logs to stderr (rate-limited) and the wrapper fails
 *     with the upgrade's errno (posix_fallocate returns it), so Postgres
 *     reports an ERROR instead of writing through a read-only fd.
 *  H7 The resolver runs from a constructor and every wrapper tolerates being
 *     called before it (libc and other libraries open files during their own
 *     initialisation); a raw syscall covers any pointer not resolved yet.
 *
 * Environment:
 *   PGOVERLAY_LAZYRW_DEBUG=1   log every downgrade and upgrade to stderr.
 *
 * Built reproducibly for glibc and musl on x86_64 and aarch64 by
 * hack/build-lazyrw.sh; the committed builds live in dist/.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/sendfile.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <time.h>
#include <unistd.h>

/* musl before 1.2.4 mapped the LFS64 names to the plain ones with macros. */
#undef open64
#undef openat64
#undef pwrite64
#undef pwritev64
#undef pwritev64v2
#undef ftruncate64
#undef truncate64
#undef fallocate64
#undef posix_fallocate64
#undef sendfile64
#undef mmap64
#undef fcntl64

#ifndef SYS_close_range
#define SYS_close_range 436 /* same number on x86_64 and the generic table */
#endif
#ifndef CLOSE_RANGE_UNSHARE
#define CLOSE_RANGE_UNSHARE (1U << 1)
#endif
#ifndef CLOSE_RANGE_CLOEXEC
#define CLOSE_RANGE_CLOEXEC (1U << 2)
#endif
#ifndef O_TMPFILE
#define O_TMPFILE 0
#endif
#ifndef O_PATH
#define O_PATH 0
#endif
#ifndef O_NOATIME
#define O_NOATIME 0
#endif
#ifndef O_DIRECT
#define O_DIRECT 0
#endif

#define EXPORT __attribute__((visibility("default")))

/* ----- fd tracking table -------------------------------------------------- */

#define MAXFD 65536

enum { ST_UNTRACKED = 0, ST_DOWNGRADED = 1, ST_UPGRADED = 2 };

typedef struct {
    int   state;      /* ST_*; read without the lock on the fast paths */
    int   orig_flags; /* the flags the caller asked for */
    dev_t dev;        /* identity at downgrade time (H2) */
    ino_t ino;
    char *path;       /* the path as the caller passed it */
} slot_t;

static slot_t g_tab[MAXFD];
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;

static int    g_active;              /* H4: postgres with PGDATA set */
static int    g_debug;
static char   g_pgdata[PATH_MAX];
static size_t g_pgdata_len;

static inline int slot_state(int fd) {
    if (fd < 0 || fd >= MAXFD) return ST_UNTRACKED;
    return __atomic_load_n(&g_tab[fd].state, __ATOMIC_ACQUIRE);
}

/* ----- the functions being wrapped ---------------------------------------- */

static int     (*real_openat)(int, const char *, int, ...);
static int     (*real_openat64)(int, const char *, int, ...);
static ssize_t (*real_write)(int, const void *, size_t);
static ssize_t (*real_pwrite)(int, const void *, size_t, off_t);
static ssize_t (*real_pwrite64)(int, const void *, size_t, off_t);
static ssize_t (*real_writev)(int, const struct iovec *, int);
static ssize_t (*real_pwritev)(int, const struct iovec *, int, off_t);
static ssize_t (*real_pwritev64)(int, const struct iovec *, int, off_t);
static ssize_t (*real_pwritev2)(int, const struct iovec *, int, off_t, int);
static ssize_t (*real_pwritev64v2)(int, const struct iovec *, int, off_t, int);
static int     (*real_ftruncate)(int, off_t);
static int     (*real_ftruncate64)(int, off_t);
static int     (*real_truncate)(const char *, off_t);
static int     (*real_truncate64)(const char *, off_t);
static int     (*real_fallocate)(int, int, off_t, off_t);
static int     (*real_fallocate64)(int, int, off_t, off_t);
static int     (*real_posix_fallocate)(int, off_t, off_t);
static int     (*real_posix_fallocate64)(int, off_t, off_t);
static ssize_t (*real_copy_file_range)(int, off_t *, int, off_t *, size_t, unsigned int);
static ssize_t (*real_sendfile)(int, int, off_t *, size_t);
static ssize_t (*real_sendfile64)(int, int, off_t *, size_t);
static ssize_t (*real_splice)(int, off_t *, int, off_t *, size_t, unsigned int);
static void   *(*real_mmap)(void *, size_t, int, int, int, off_t);
static void   *(*real_mmap64)(void *, size_t, int, int, int, off_t);
static int     (*real_close)(int);
static int     (*real_close_range)(unsigned int, unsigned int, int);
static void    (*real_closefrom)(int);
static int     (*real_dup)(int);
static int     (*real_dup2)(int, int);
static int     (*real_dup3)(int, int, int);
static int     (*real_fcntl)(int, int, ...);
static int     (*real_fcntl64)(int, int, ...);

/* 0 = not resolved, 1 = resolving, 2 = resolved. A call made while resolving
 * (dlsym can open files) sees NULL pointers and takes the syscall fallback. */
static int g_resolved;

#define RESOLVE(name) \
    __atomic_store_n(&real_##name, (__typeof__(real_##name))dlsym(RTLD_NEXT, #name), __ATOMIC_RELAXED)

static void init_reals(void) {
    int expected = 0;
    if (__atomic_load_n(&g_resolved, __ATOMIC_ACQUIRE) == 2) return;
    if (!__atomic_compare_exchange_n(&g_resolved, &expected, 1, 0, __ATOMIC_ACQ_REL, __ATOMIC_ACQUIRE))
        return;
    RESOLVE(openat); RESOLVE(openat64);
    RESOLVE(write); RESOLVE(pwrite); RESOLVE(pwrite64); RESOLVE(writev);
    RESOLVE(pwritev); RESOLVE(pwritev64); RESOLVE(pwritev2); RESOLVE(pwritev64v2);
    RESOLVE(ftruncate); RESOLVE(ftruncate64); RESOLVE(truncate); RESOLVE(truncate64);
    RESOLVE(fallocate); RESOLVE(fallocate64);
    RESOLVE(posix_fallocate); RESOLVE(posix_fallocate64);
    RESOLVE(copy_file_range); RESOLVE(sendfile); RESOLVE(sendfile64); RESOLVE(splice);
    RESOLVE(mmap); RESOLVE(mmap64);
    RESOLVE(close); RESOLVE(close_range); RESOLVE(closefrom);
    RESOLVE(dup); RESOLVE(dup2); RESOLVE(dup3); RESOLVE(fcntl); RESOLVE(fcntl64);
    __atomic_store_n(&g_resolved, 2, __ATOMIC_RELEASE);
}

/* Load a pointer, preferring the first non-NULL of two (the *64 aliases). */
#define REAL(name) __atomic_load_n(&real_##name, __ATOMIC_RELAXED)

/* ----- raw helpers that never re-enter the wrappers ----------------------- */

static int raw_openat(int dfd, const char *p, int flags, mode_t mode) {
    int (*f)(int, const char *, int, ...) = REAL(openat);
    if (f) return f(dfd, p, flags, mode);
    return (int)syscall(SYS_openat, dfd, p, flags, mode);
}

static int raw_close(int fd) {
    int (*f)(int) = REAL(close);
    return f ? f(fd) : (int)syscall(SYS_close, fd);
}

static int raw_fcntl(int fd, int cmd, long arg) {
    int (*f)(int, int, ...) = REAL(fcntl);
    return f ? f(fd, cmd, arg) : (int)syscall(SYS_fcntl, fd, cmd, arg);
}

static int raw_dup3(int oldfd, int newfd, int flags) {
    int (*f)(int, int, int) = REAL(dup3);
    return f ? f(oldfd, newfd, flags) : (int)syscall(SYS_dup3, oldfd, newfd, flags);
}

static int raw_dup2(int oldfd, int newfd) {
    int (*f)(int, int) = REAL(dup2);
    if (f) return f(oldfd, newfd);
#ifdef SYS_dup2
    return (int)syscall(SYS_dup2, oldfd, newfd);
#else
    /* aarch64 has no dup2 syscall. */
    if (oldfd == newfd) return raw_fcntl(oldfd, F_GETFD, 0) < 0 ? -1 : newfd;
    return (int)syscall(SYS_dup3, oldfd, newfd, 0);
#endif
}

/* ----- logging ------------------------------------------------------------ */

static void emit(const char *buf, int n) {
    if (n <= 0) return;
    if (n > 1023) n = 1023;
    long r = syscall(SYS_write, 2, buf, (size_t)n);
    (void)r;
}

static void dbg(const char *fmt, ...) {
    if (!g_debug) return;
    char buf[1024];
    va_list ap;
    va_start(ap, fmt);
    int n = vsnprintf(buf, sizeof buf, fmt, ap);
    va_end(ap);
    emit(buf, n);
}

/* H6: at most one line per second per process, with a count of the rest. */
static void log_upgrade_failure(int fd, const char *path, const char *step, int err) {
    static long last = -1;
    static unsigned suppressed;
    struct timespec ts;
    if (clock_gettime(CLOCK_MONOTONIC, &ts) != 0) ts.tv_sec = 0;
    if (__atomic_load_n(&last, __ATOMIC_RELAXED) == (long)ts.tv_sec) {
        __atomic_add_fetch(&suppressed, 1, __ATOMIC_RELAXED);
        return;
    }
    __atomic_store_n(&last, (long)ts.tv_sec, __ATOMIC_RELAXED);
    unsigned s = __atomic_exchange_n(&suppressed, 0, __ATOMIC_RELAXED);
    char buf[1024];
    int n = snprintf(buf, sizeof buf,
                     "pgoverlay-lazyrw: pid %ld: could not upgrade fd %d (%s) to read-write: %s: %s (errno %d)"
                     "; the write fails%s\n",
                     (long)getpid(), fd, path ? path : "?", step, strerror(err), err,
                     s ? " (similar messages suppressed)" : "");
    emit(buf, n);
}

/* ----- path policy ---------------------------------------------------------
 * Downgrade write-intent opens of relation and SLRU files under PGDATA.
 * Postgres chdir()s into the data directory at start-up, so relation paths
 * arrive relative ("base/16384/16385", "global/1259", "pg_xact/0000");
 * absolute paths under PGDATA are matched too. The WAL is never downgraded:
 * it is written anyway.
 */
static const char *const k_data_dirs[] = {
    "base/", "global/", "pg_tblspc/",
    "pg_xact/", "pg_multixact/", "pg_subtrans/", "pg_commit_ts/",
    "pg_serial/", "pg_snapshots/", "pg_logical/",
    NULL,
};

/* Returns the part of path relative to the data directory, or NULL when the
 * path is not (predictably) inside it. */
static const char *pgdata_relative(const char *p) {
    if (!p || !*p) return NULL;
    if (p[0] != '/') {
        while (p[0] == '.' && p[1] == '/') p += 2;
        return p; /* relative: the server's cwd is the data directory */
    }
    if (g_pgdata_len && strncmp(p, g_pgdata, g_pgdata_len) == 0 && p[g_pgdata_len] == '/') {
        p += g_pgdata_len;
        while (*p == '/') p++;
        return p;
    }
    return NULL;
}

static int is_data_path(const char *p) {
    const char *rel = pgdata_relative(p);
    if (!rel) return 0;
    for (int i = 0; k_data_dirs[i]; i++)
        if (strncmp(rel, k_data_dirs[i], strlen(k_data_dirs[i])) == 0) return 1;
    return 0;
}

static int wants_downgrade(int dirfd, const char *path, int flags) {
    int acc = flags & O_ACCMODE;
    if (!g_active) return 0;
    if (acc != O_RDWR && acc != O_WRONLY) return 0;          /* already read-only */
    if (flags & (O_CREAT | O_TRUNC | O_PATH | O_DIRECTORY)) return 0;
    if (O_TMPFILE && (flags & O_TMPFILE) == O_TMPFILE) return 0;
    /* Only paths whose resolution is predictable: absolute, or cwd-relative. */
    if (!path || (path[0] != '/' && dirfd != AT_FDCWD)) return 0;
    return is_data_path(path);
}

/* ----- tracking ------------------------------------------------------------ */

static void slot_clear_locked(int fd) {
    slot_t *s = &g_tab[fd];
    free(s->path);
    s->path = NULL;
    s->orig_flags = 0;
    s->dev = 0;
    s->ino = 0;
    __atomic_store_n(&s->state, ST_UNTRACKED, __ATOMIC_RELEASE);
}

static void forget(int fd) {
    if (slot_state(fd) == ST_UNTRACKED) return; /* lock-free fast path */
    pthread_mutex_lock(&g_lock);
    slot_clear_locked(fd);
    pthread_mutex_unlock(&g_lock);
}

static void forget_range(unsigned int first, unsigned int last) {
    if (first >= MAXFD) return;
    if (last >= MAXFD) last = MAXFD - 1;
    for (unsigned int fd = first; fd <= last; fd++) forget((int)fd);
}

/* Record a downgraded fd. Returns 0, or -1 if it cannot be tracked. */
static int track(int fd, const char *path, int orig_flags, const struct stat *st) {
    if (fd < 0 || fd >= MAXFD) return -1;
    char *copy = strdup(path);
    if (!copy) return -1;
    pthread_mutex_lock(&g_lock);
    slot_t *s = &g_tab[fd];
    free(s->path);
    s->path = copy;
    s->orig_flags = orig_flags;
    s->dev = st->st_dev;
    s->ino = st->st_ino;
    __atomic_store_n(&s->state, ST_DOWNGRADED, __ATOMIC_RELEASE);
    pthread_mutex_unlock(&g_lock);
    return 0;
}

/* dst now refers to the same open file description as src. A downgraded src
 * makes dst downgraded too; each fd then upgrades itself on its own first
 * write (so, unlike a real dup, the two stop sharing an offset after that). */
static void copy_track(int src, int dst) {
    if (dst < 0 || dst >= MAXFD || src == dst) return;
    if (slot_state(src) != ST_DOWNGRADED && slot_state(dst) == ST_UNTRACKED) return;
    pthread_mutex_lock(&g_lock);
    slot_clear_locked(dst);
    if (src >= 0 && src < MAXFD && g_tab[src].state == ST_DOWNGRADED && g_tab[src].path) {
        char *copy = strdup(g_tab[src].path);
        if (copy) {
            g_tab[dst].path = copy;
            g_tab[dst].orig_flags = g_tab[src].orig_flags;
            g_tab[dst].dev = g_tab[src].dev;
            g_tab[dst].ino = g_tab[src].ino;
            __atomic_store_n(&g_tab[dst].state, ST_DOWNGRADED, __ATOMIC_RELEASE);
        }
        /* strdup failure leaves dst untracked: its writes then fail with
         * EBADF, loudly, instead of going anywhere wrong. */
    }
    pthread_mutex_unlock(&g_lock);
}

/* H2: is fd still the file recorded in its slot? */
static int still_ours_locked(int fd, const slot_t *s) {
    struct stat st, pst;
    if (fstat(fd, &st) != 0 || !S_ISREG(st.st_mode)) return 0;
    int fl = raw_fcntl(fd, F_GETFL, 0);
    if (fl < 0 || (fl & O_ACCMODE) != O_RDONLY) return 0;
    if (st.st_dev == s->dev && st.st_ino == s->ino) return 1;
    /* A copy-up by another process can change what OverlayFS reports for a
     * file (layers on different filesystems without origin tracking). Then
     * accept the fd if it is the file the path names now: that is the file
     * the upgrade reopens. */
    return stat(s->path, &pst) == 0 && pst.st_dev == st.st_dev && pst.st_ino == st.st_ino;
}

#define STATUS_FLAGS (O_APPEND | O_NONBLOCK | O_DIRECT | O_NOATIME | O_ASYNC)

/*
 * Reopen a downgraded fd's path with the caller's original flags and swap the
 * new description onto the same fd number. With trunc, the reopen adds O_TRUNC
 * (H1: OverlayFS then copies up nothing). Returns 0 when the fd is ready for
 * the call (upgraded now, earlier, or not ours to touch), -1 with errno set
 * when the upgrade failed and the call must fail.
 */
static int upgrade(int fd, int trunc) {
    if (slot_state(fd) != ST_DOWNGRADED) return 0;
    pthread_mutex_lock(&g_lock);
    slot_t *s = &g_tab[fd];
    if (s->state != ST_DOWNGRADED || !s->path) {
        pthread_mutex_unlock(&g_lock);
        return 0;
    }
    if (!still_ours_locked(fd, s)) {
        dbg("[lazyrw] stale slot fd=%d path=%s: forgotten, call passed through\n", fd, s->path);
        slot_clear_locked(fd);
        pthread_mutex_unlock(&g_lock);
        return 0;
    }
    int fdflags = raw_fcntl(fd, F_GETFD, 0);
    int fl = raw_fcntl(fd, F_GETFL, 0);
    off_t off = lseek(fd, 0, SEEK_CUR);

    int oflags = (s->orig_flags & ~(O_CREAT | O_EXCL | O_TRUNC | O_NOCTTY)) | O_CLOEXEC;
    if (trunc) oflags |= O_TRUNC;
    int nfd = raw_openat(AT_FDCWD, s->path, oflags, 0);
    if (nfd < 0) {
        int err = errno;
        log_upgrade_failure(fd, s->path, "reopen", err);
        pthread_mutex_unlock(&g_lock);
        errno = err;
        return -1;
    }
    if (fl >= 0) {
        int nfl = raw_fcntl(nfd, F_GETFL, 0);
        if (nfl >= 0 && ((nfl ^ fl) & STATUS_FLAGS))
            raw_fcntl(nfd, F_SETFL, (nfl & ~STATUS_FLAGS) | (fl & STATUS_FLAGS));
    }
    if (off != (off_t)-1 && lseek(nfd, off, SEEK_SET) == (off_t)-1) {
        int err = errno;
        raw_close(nfd);
        log_upgrade_failure(fd, s->path, "restore offset", err);
        pthread_mutex_unlock(&g_lock);
        errno = err;
        return -1;
    }
    int cloexec = fdflags >= 0 && (fdflags & FD_CLOEXEC);
    if (raw_dup3(nfd, fd, cloexec ? O_CLOEXEC : 0) < 0) {
        int err = errno;
        raw_close(nfd);
        log_upgrade_failure(fd, s->path, "dup3", err);
        pthread_mutex_unlock(&g_lock);
        errno = err;
        return -1;
    }
    raw_close(nfd);
    dbg("[lazyrw] COPY-UP on first write fd=%d path=%s%s\n", fd, s->path, trunc ? " (O_TRUNC)" : "");
    free(s->path);
    s->path = NULL;
    __atomic_store_n(&s->state, ST_UPGRADED, __ATOMIC_RELEASE);
    pthread_mutex_unlock(&g_lock);
    return 0;
}

/* Upgrade fd if it was downgraded; on failure return failret from the caller
 * with errno set by the upgrade. */
#define UPGRADE_OR_FAIL(fd, failret)                                           \
    do {                                                                       \
        if (slot_state(fd) == ST_DOWNGRADED && upgrade((fd), 0) < 0)           \
            return (failret);                                                  \
    } while (0)

/* ----- process set-up (H4, H7) --------------------------------------------- */

static void atfork_prepare(void) { pthread_mutex_lock(&g_lock); }
static void atfork_release(void) { pthread_mutex_unlock(&g_lock); }

__attribute__((constructor)) static void lazyrw_init(void) {
    init_reals();
    const char *dbgenv = getenv("PGOVERLAY_LAZYRW_DEBUG");
    g_debug = dbgenv && *dbgenv && strcmp(dbgenv, "0") != 0;

    /* Decide once, now: postgres's ps_status later overwrites the argv area
     * that program_invocation_short_name points into, and moves environ. */
    const char *name = program_invocation_short_name;
    const char *pgdata = getenv("PGDATA");
    if (!name || strcmp(name, "postgres") != 0 || !pgdata || pgdata[0] != '/') return;
    size_t n = strlen(pgdata);
    while (n > 1 && pgdata[n - 1] == '/') n--;
    if (n >= sizeof g_pgdata) return;
    memcpy(g_pgdata, pgdata, n);
    g_pgdata[n] = '\0';
    g_pgdata_len = n;
    pthread_atfork(atfork_prepare, atfork_release, atfork_release);
    __atomic_store_n(&g_active, 1, __ATOMIC_RELEASE);
    dbg("[lazyrw] active pid=%ld PGDATA=%s\n", (long)getpid(), g_pgdata);
}

/* Test hooks (internal/cow/lazyrw/test): the tracking state of an fd
 * (0 untracked, 1 downgraded, 2 upgraded, -1 out of range) and whether the
 * shim is active in this process. */
EXPORT int pgoverlay_lazyrw_state(int fd) {
    if (fd < 0 || fd >= MAXFD) return -1;
    return slot_state(fd);
}
EXPORT int pgoverlay_lazyrw_active(void) { return __atomic_load_n(&g_active, __ATOMIC_ACQUIRE); }

/* ----- open family ----------------------------------------------------------- */

static int do_openat(int dirfd, const char *path, int flags, mode_t mode) {
    init_reals();
    if (!wants_downgrade(dirfd, path, flags)) return raw_openat(dirfd, path, flags, mode);

    int fd = raw_openat(dirfd, path, (flags & ~O_ACCMODE) | O_RDONLY, 0);
    if (fd < 0) return fd;
    struct stat st;
    int saved = errno;
    if (fd < MAXFD && fstat(fd, &st) == 0 && S_ISREG(st.st_mode) && track(fd, path, flags, &st) == 0) {
        dbg("[lazyrw] downgrade->RDONLY fd=%d path=%s\n", fd, path);
        errno = saved;
        return fd;
    }
    /* H3 (and anything else that cannot be tracked): open it the way the
     * caller asked. The copy-up happens now, which is always correct. */
    raw_close(fd);
    errno = saved;
    return raw_openat(dirfd, path, flags, mode);
}

static mode_t mode_arg(int flags, va_list ap) {
    if ((flags & O_CREAT) || (O_TMPFILE && (flags & O_TMPFILE) == O_TMPFILE)) return (mode_t)va_arg(ap, int);
    return 0;
}

EXPORT int open(const char *path, int flags, ...) {
    va_list ap;
    va_start(ap, flags);
    mode_t mode = mode_arg(flags, ap);
    va_end(ap);
    return do_openat(AT_FDCWD, path, flags, mode);
}
EXPORT int open64(const char *path, int flags, ...) {
    va_list ap;
    va_start(ap, flags);
    mode_t mode = mode_arg(flags, ap);
    va_end(ap);
    return do_openat(AT_FDCWD, path, flags, mode);
}
EXPORT int openat(int dirfd, const char *path, int flags, ...) {
    va_list ap;
    va_start(ap, flags);
    mode_t mode = mode_arg(flags, ap);
    va_end(ap);
    return do_openat(dirfd, path, flags, mode);
}
EXPORT int openat64(int dirfd, const char *path, int flags, ...) {
    va_list ap;
    va_start(ap, flags);
    mode_t mode = mode_arg(flags, ap);
    va_end(ap);
    return do_openat(dirfd, path, flags, mode);
}
/* _FORTIFY_SOURCE variants, used when the caller passes no mode. */
EXPORT int __open_2(const char *path, int flags) { return do_openat(AT_FDCWD, path, flags, 0); }
EXPORT int __open64_2(const char *path, int flags) { return do_openat(AT_FDCWD, path, flags, 0); }
EXPORT int __openat_2(int dfd, const char *path, int flags) { return do_openat(dfd, path, flags, 0); }
EXPORT int __openat64_2(int dfd, const char *path, int flags) { return do_openat(dfd, path, flags, 0); }

/* ----- write-class calls: upgrade first ------------------------------------ */

EXPORT ssize_t write(int fd, const void *buf, size_t n) {
    init_reals();
    UPGRADE_OR_FAIL(fd, -1);
    ssize_t (*f)(int, const void *, size_t) = REAL(write);
    return f ? f(fd, buf, n) : syscall(SYS_write, fd, buf, n);
}

static ssize_t do_pwrite(int fd, const void *buf, size_t n, off_t o) {
    init_reals();
    UPGRADE_OR_FAIL(fd, -1);
    ssize_t (*f)(int, const void *, size_t, off_t) = REAL(pwrite);
    if (!f) f = REAL(pwrite64);
    return f ? f(fd, buf, n, o) : syscall(SYS_pwrite64, fd, buf, n, o);
}
EXPORT ssize_t pwrite(int fd, const void *buf, size_t n, off_t o) { return do_pwrite(fd, buf, n, o); }
EXPORT ssize_t pwrite64(int fd, const void *buf, size_t n, off_t o) { return do_pwrite(fd, buf, n, o); }

EXPORT ssize_t writev(int fd, const struct iovec *v, int c) {
    init_reals();
    UPGRADE_OR_FAIL(fd, -1);
    ssize_t (*f)(int, const struct iovec *, int) = REAL(writev);
    return f ? f(fd, v, c) : syscall(SYS_writev, fd, v, c);
}

static ssize_t do_pwritev(int fd, const struct iovec *v, int c, off_t o) {
    init_reals();
    UPGRADE_OR_FAIL(fd, -1);
    ssize_t (*f)(int, const struct iovec *, int, off_t) = REAL(pwritev);
    if (!f) f = REAL(pwritev64);
    /* 64-bit only: the high half of the split offset is 0. */
    return f ? f(fd, v, c, o) : syscall(SYS_pwritev, fd, v, c, o, 0);
}
EXPORT ssize_t pwritev(int fd, const struct iovec *v, int c, off_t o) { return do_pwritev(fd, v, c, o); }
EXPORT ssize_t pwritev64(int fd, const struct iovec *v, int c, off_t o) { return do_pwritev(fd, v, c, o); }

static ssize_t do_pwritev2(int fd, const struct iovec *v, int c, off_t o, int fl) {
    init_reals();
    UPGRADE_OR_FAIL(fd, -1);
    ssize_t (*f)(int, const struct iovec *, int, off_t, int) = REAL(pwritev2);
    if (!f) f = REAL(pwritev64v2);
    return f ? f(fd, v, c, o, fl) : syscall(SYS_pwritev2, fd, v, c, o, 0, fl);
}
EXPORT ssize_t pwritev2(int fd, const struct iovec *v, int c, off_t o, int fl) { return do_pwritev2(fd, v, c, o, fl); }
EXPORT ssize_t pwritev64v2(int fd, const struct iovec *v, int c, off_t o, int fl) { return do_pwritev2(fd, v, c, o, fl); }

/* H1: ftruncate(fd, 0) of a downgraded fd reopens with O_TRUNC. */
static int do_ftruncate(int fd, off_t len) {
    init_reals();
    if (slot_state(fd) == ST_DOWNGRADED && upgrade(fd, len == 0) < 0) return -1;
    int (*f)(int, off_t) = REAL(ftruncate);
    if (!f) f = REAL(ftruncate64);
    return f ? f(fd, len) : (int)syscall(SYS_ftruncate, fd, len);
}
EXPORT int ftruncate(int fd, off_t len) { return do_ftruncate(fd, len); }
EXPORT int ftruncate64(int fd, off_t len) { return do_ftruncate(fd, len); }

/* H1: truncate(path, 0) of a data file is open(O_WRONLY|O_TRUNC) + close. */
static int do_truncate(const char *path, off_t len) {
    init_reals();
    if (g_active && len == 0 && is_data_path(path)) {
        int fd = raw_openat(AT_FDCWD, path, O_WRONLY | O_TRUNC | O_CLOEXEC | O_NOCTTY, 0);
        if (fd < 0) return -1;
        dbg("[lazyrw] truncate(0) as O_TRUNC open path=%s\n", path);
        return raw_close(fd) == 0 || errno == EINTR ? 0 : -1;
    }
    int (*f)(const char *, off_t) = REAL(truncate);
    if (!f) f = REAL(truncate64);
    return f ? f(path, len) : (int)syscall(SYS_truncate, path, len);
}
EXPORT int truncate(const char *path, off_t len) { return do_truncate(path, len); }
EXPORT int truncate64(const char *path, off_t len) { return do_truncate(path, len); }

static int do_fallocate(int fd, int mode, off_t off, off_t len) {
    init_reals();
    UPGRADE_OR_FAIL(fd, -1);
    int (*f)(int, int, off_t, off_t) = REAL(fallocate);
    if (!f) f = REAL(fallocate64);
    return f ? f(fd, mode, off, len) : (int)syscall(SYS_fallocate, fd, mode, off, len);
}
EXPORT int fallocate(int fd, int mode, off_t off, off_t len) { return do_fallocate(fd, mode, off, len); }
EXPORT int fallocate64(int fd, int mode, off_t off, off_t len) { return do_fallocate(fd, mode, off, len); }

/* posix_fallocate reports errors as its return value, not through errno. */
static int do_posix_fallocate(int fd, off_t off, off_t len) {
    init_reals();
    if (slot_state(fd) == ST_DOWNGRADED && upgrade(fd, 0) < 0) return errno;
    int (*f)(int, off_t, off_t) = REAL(posix_fallocate);
    if (!f) f = REAL(posix_fallocate64);
    if (f) return f(fd, off, len);
    return syscall(SYS_fallocate, fd, 0, off, len) == 0 ? 0 : errno;
}
EXPORT int posix_fallocate(int fd, off_t off, off_t len) { return do_posix_fallocate(fd, off, len); }
EXPORT int posix_fallocate64(int fd, off_t off, off_t len) { return do_posix_fallocate(fd, off, len); }

EXPORT ssize_t copy_file_range(int in, off_t *inoff, int out, off_t *outoff, size_t n, unsigned int fl) {
    init_reals();
    UPGRADE_OR_FAIL(out, -1);
    ssize_t (*f)(int, off_t *, int, off_t *, size_t, unsigned int) = REAL(copy_file_range);
    return f ? f(in, inoff, out, outoff, n, fl) : syscall(SYS_copy_file_range, in, inoff, out, outoff, n, fl);
}

static ssize_t do_sendfile(int out, int in, off_t *off, size_t n) {
    init_reals();
    UPGRADE_OR_FAIL(out, -1);
    ssize_t (*f)(int, int, off_t *, size_t) = REAL(sendfile);
    if (!f) f = REAL(sendfile64);
    return f ? f(out, in, off, n) : syscall(SYS_sendfile, out, in, off, n);
}
EXPORT ssize_t sendfile(int out, int in, off_t *off, size_t n) { return do_sendfile(out, in, off, n); }
EXPORT ssize_t sendfile64(int out, int in, off_t *off, size_t n) { return do_sendfile(out, in, off, n); }

EXPORT ssize_t splice(int in, off_t *inoff, int out, off_t *outoff, size_t n, unsigned int fl) {
    init_reals();
    UPGRADE_OR_FAIL(out, -1);
    ssize_t (*f)(int, off_t *, int, off_t *, size_t, unsigned int) = REAL(splice);
    return f ? f(in, inoff, out, outoff, n, fl) : syscall(SYS_splice, in, inoff, out, outoff, n, fl);
}

/* A shared writable mapping of a read-only fd fails with EACCES: upgrade
 * first. Postgres does not map relation files; this keeps the shim honest. */
static void *do_mmap(void *addr, size_t len, int prot, int flags, int fd, off_t off) {
    init_reals();
    if ((prot & PROT_WRITE) && (flags & MAP_SHARED)) UPGRADE_OR_FAIL(fd, MAP_FAILED);
    void *(*f)(void *, size_t, int, int, int, off_t) = REAL(mmap);
    if (!f) f = REAL(mmap64);
    return f ? f(addr, len, prot, flags, fd, off) : (void *)syscall(SYS_mmap, addr, len, prot, flags, fd, off);
}
EXPORT void *mmap(void *addr, size_t len, int prot, int flags, int fd, off_t off) {
    return do_mmap(addr, len, prot, flags, fd, off);
}
EXPORT void *mmap64(void *addr, size_t len, int prot, int flags, int fd, off_t off) {
    return do_mmap(addr, len, prot, flags, fd, off);
}

/* ----- fd lifecycle ---------------------------------------------------------- */

EXPORT int close(int fd) {
    init_reals();
    forget(fd);
    return raw_close(fd);
}

EXPORT int close_range(unsigned int first, unsigned int last, int flags) {
    init_reals();
    int (*f)(unsigned int, unsigned int, int) = REAL(close_range);
    int r = f ? f(first, last, flags) : (int)syscall(SYS_close_range, first, last, flags);
    if (r == 0 && !((unsigned int)flags & CLOSE_RANGE_CLOEXEC)) forget_range(first, last);
    return r;
}

EXPORT void closefrom(int lowfd) {
    init_reals();
    if (lowfd < 0) lowfd = 0;
    void (*f)(int) = REAL(closefrom);
    if (f) {
        f(lowfd);
    } else if (syscall(SYS_close_range, (unsigned int)lowfd, ~0U, 0) != 0) {
        struct rlimit rl;
        long max = getrlimit(RLIMIT_NOFILE, &rl) == 0 && rl.rlim_cur != RLIM_INFINITY ? (long)rl.rlim_cur : MAXFD;
        for (long fd = lowfd; fd < max; fd++) raw_close((int)fd);
    }
    forget_range((unsigned int)lowfd, ~0U);
}

EXPORT int dup(int oldfd) {
    init_reals();
    int (*f)(int) = REAL(dup);
    int n = f ? f(oldfd) : (int)syscall(SYS_dup, oldfd);
    if (n >= 0) copy_track(oldfd, n);
    return n;
}

EXPORT int dup2(int oldfd, int newfd) {
    init_reals();
    int n = raw_dup2(oldfd, newfd);
    if (n >= 0) copy_track(oldfd, n);
    return n;
}

EXPORT int dup3(int oldfd, int newfd, int flags) {
    init_reals();
    int n = raw_dup3(oldfd, newfd, flags);
    if (n >= 0) copy_track(oldfd, n);
    return n;
}

static int do_fcntl(int (*f)(int, int, ...), int fd, int cmd, void *arg) {
    int r = f ? f(fd, cmd, arg) : (int)syscall(SYS_fcntl, fd, cmd, arg);
    if (r >= 0 && (cmd == F_DUPFD || cmd == F_DUPFD_CLOEXEC)) copy_track(fd, r);
    return r;
}

/* Every fcntl argument is an int, a long or a pointer; on the 64-bit targets
 * all of them are read back correctly as a pointer-sized value (glibc's own
 * fcntl does the same). */
EXPORT int fcntl(int fd, int cmd, ...) {
    va_list ap;
    va_start(ap, cmd);
    void *arg = va_arg(ap, void *);
    va_end(ap);
    init_reals();
    return do_fcntl(REAL(fcntl), fd, cmd, arg);
}
EXPORT int fcntl64(int fd, int cmd, ...) {
    va_list ap;
    va_start(ap, cmd);
    void *arg = va_arg(ap, void *);
    va_end(ap);
    init_reals();
    int (*f)(int, int, ...) = REAL(fcntl64);
    return do_fcntl(f ? f : REAL(fcntl), fd, cmd, arg);
}
