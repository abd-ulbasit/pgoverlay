// SPDX-License-Identifier: Apache-2.0
//
// pgoverlay-du: disk usage of a directory tree that tells reflink-shared
// extents apart from the ones the tree owns alone.
//
// pgoverlay measures a branch's writable layer to report what the branch
// costs. On a filesystem that reflinks (XFS with reflink=1, btrfs), OverlayFS
// copy-up clones a file instead of copying it: the upper copy shares every
// extent with the source until a block is rewritten. `du` counts the whole
// file either way, so it reports a cloned 1 GiB segment with one changed page
// as 1 GiB. This tool asks the filesystem (FS_IOC_FIEMAP) which extents are
// shared and counts only the others as the branch's own.
//
// Usage:
//
//   pgoverlay-du PATH...
//       For each PATH, prints one line:
//         <exclusive>\t<shared>\t<apparent>\t<PATH>
//       exclusive  bytes in extents FIEMAP does not flag FIEMAP_EXTENT_SHARED,
//                  plus the allocation of directories and symlinks, and of
//                  files FIEMAP cannot map (tmpfs, some network filesystems)
//       shared     bytes in extents flagged FIEMAP_EXTENT_SHARED (reflink
//                  clones, deduplicated or snapshotted extents)
//       apparent   the sum of st_size, which is what `du -sb` prints
//       The first field is what pgoverlay reports as the branch's usage, so
//       the output parses like `du -sb`. The walk stays on PATH's
//       filesystem, never follows symlinks, counts a hard-linked inode once,
//       and skips entries that vanish while it runs (a live Postgres deletes
//       files).
//
//   pgoverlay-du -c BYTES DIR
//       Sets the XFS copy-on-write extent size hint of DIR (FS_XFLAG_COWEXTSIZE
//       with fsx_cowextsize = BYTES; 0 clears it). Files and directories
//       created under DIR afterwards inherit it, so the first write to a
//       cloned extent copies BYTES instead of the filesystem default (32
//       blocks, 128 KiB with 4 KiB blocks). Prints "cowextsize=<bytes>\t<DIR>"
//       as the filesystem reports it back. A filesystem without the hint
//       fails with the ioctl's error.
//
//   pgoverlay-du -V
//       Prints the tool's version.
//
// Exit status: 0 on success; 1 when some entries could not be measured (the
// totals are still printed, and every failure is reported on stderr); 2 on a
// usage error or when a PATH itself cannot be read.
//
// Build (static; needs only libc and the Linux UAPI headers):
//   cc -static -O2 -Wall -Wextra -o pgoverlay-du pgoverlay-du.c && strip pgoverlay-du
// The release binaries for linux/amd64 and linux/arm64 are built by the same
// script that builds the lazyrw shim and embedded into pgoverlay from
// internal/cow/usage/dist (see internal/cow/usage.go and dist/README.md).

#define _GNU_SOURCE
#include <dirent.h>
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <limits.h>
#include <linux/fiemap.h>
#include <linux/fs.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#define PGOVERLAY_DU_VERSION "1"

// Older kernel headers lack the fsxattr interface (Linux 4.5) or the CoW
// extent size flag (4.9); the values are ABI and never change.
#ifndef FS_IOC_FSGETXATTR
struct fsxattr {
	uint32_t fsx_xflags;
	uint32_t fsx_extsize;
	uint32_t fsx_nextents;
	uint32_t fsx_projid;
	uint32_t fsx_cowextsize;
	unsigned char fsx_pad[8];
};
#define FS_IOC_FSGETXATTR _IOR('X', 31, struct fsxattr)
#define FS_IOC_FSSETXATTR _IOW('X', 32, struct fsxattr)
#endif
#ifndef FS_XFLAG_COWEXTSIZE
#define FS_XFLAG_COWEXTSIZE 0x00010000
#endif
#ifndef FIEMAP_EXTENT_SHARED
#define FIEMAP_EXTENT_SHARED 0x00002000
#endif
#ifndef O_NOATIME
#define O_NOATIME 0
#endif

// Extents fetched per FIEMAP call. A 1 GiB Postgres segment that was cloned
// and then written in scattered places can have thousands of extents; the
// loop in measure_file pages through them.
#define EXTENTS_PER_CALL 512

// A tree deeper than this is reported as an error instead of risking running
// out of file descriptors (every level holds its directory open). A Postgres
// data directory is about five levels deep.
#define MAX_DEPTH 128

struct totals {
	uint64_t exclusive;
	uint64_t shared;
	uint64_t apparent;
};

static const char *prog = "pgoverlay-du";
static int errors;
static struct fiemap *fm;

// ---- hard links: the (dev, ino) of every inode with st_nlink > 1 counted so
// far, in an open-addressing hash set. Inode 0 never names a real inode, so
// it marks an empty slot.

struct inode_key {
	dev_t dev;
	ino_t ino;
};

static struct inode_key *seen;
static size_t seen_cap, seen_len;

static size_t inode_slot(dev_t dev, ino_t ino, size_t mask)
{
	uint64_t h = ((uint64_t)ino * 0x9E3779B97F4A7C15ull) ^ (uint64_t)dev;
	return (size_t)(h ^ (h >> 29)) & mask;
}

static void seen_put(struct inode_key *tab, size_t cap, dev_t dev, ino_t ino)
{
	size_t mask = cap - 1;
	for (size_t i = inode_slot(dev, ino, mask);; i = (i + 1) & mask) {
		if (tab[i].ino == 0) {
			tab[i].dev = dev;
			tab[i].ino = ino;
			return;
		}
	}
}

// seen_insert returns 1 when (dev, ino) is new, 0 when it was counted before.
static int seen_insert(dev_t dev, ino_t ino)
{
	if (seen_cap) {
		size_t mask = seen_cap - 1;
		for (size_t i = inode_slot(dev, ino, mask);; i = (i + 1) & mask) {
			if (seen[i].ino == 0)
				break;
			if (seen[i].ino == ino && seen[i].dev == dev)
				return 0;
		}
	}
	if ((seen_len + 1) * 2 > seen_cap) {
		size_t cap = seen_cap ? seen_cap * 2 : 1024;
		struct inode_key *tab = calloc(cap, sizeof *tab);
		if (!tab) {
			fprintf(stderr, "%s: out of memory\n", prog);
			exit(2);
		}
		for (size_t i = 0; i < seen_cap; i++)
			if (seen[i].ino != 0)
				seen_put(tab, cap, seen[i].dev, seen[i].ino);
		free(seen);
		seen = tab;
		seen_cap = cap;
	}
	seen_put(seen, seen_cap, dev, ino);
	seen_len++;
	return 1;
}

// ---- measurement

// vanished reports an error that means the entry was removed while the walk
// ran: not a failure, the entry simply no longer costs anything.
static int vanished(int err)
{
	return err == ENOENT || err == ESTALE;
}

static void fail(const char *path, const char *what, int err)
{
	fprintf(stderr, "%s: %s: %s: %s\n", prog, path, what, strerror(err));
	errors++;
}

// allocated is the space st_blocks says the entry occupies, in bytes.
static uint64_t allocated(const struct stat *st)
{
	return (uint64_t)st->st_blocks * 512u;
}

// measure_file adds a regular file's extents to t. A file FIEMAP cannot map
// is counted by its allocation, as exclusive.
static void measure_file(int parent, const char *name, const char *path, const struct stat *st, struct totals *t)
{
	t->apparent += (uint64_t)st->st_size;
	int flags = O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK;
	int fd = openat(parent, name, flags | O_NOATIME);
	if (fd < 0 && errno == EPERM)
		fd = openat(parent, name, flags); // O_NOATIME needs ownership or CAP_FOWNER
	if (fd < 0) {
		if (!vanished(errno))
			fail(path, "open", errno);
		return;
	}
	uint64_t start = 0, excl = 0, shared = 0;
	for (;;) {
		memset(fm, 0, sizeof *fm);
		fm->fm_start = start;
		fm->fm_length = FIEMAP_MAX_OFFSET - start;
		fm->fm_extent_count = EXTENTS_PER_CALL;
		if (ioctl(fd, FS_IOC_FIEMAP, fm) < 0) {
			int err = errno;
			if (err == EOPNOTSUPP || err == ENOTTY || err == EINVAL || err == ENOSYS) {
				excl = allocated(st); // no extent map on this filesystem
				shared = 0;
			} else if (!vanished(err)) {
				fail(path, "FS_IOC_FIEMAP", err);
			}
			break;
		}
		if (fm->fm_mapped_extents == 0)
			break;
		int last = 0;
		for (uint32_t i = 0; i < fm->fm_mapped_extents; i++) {
			const struct fiemap_extent *e = &fm->fm_extents[i];
			if (e->fe_flags & FIEMAP_EXTENT_SHARED)
				shared += e->fe_length;
			else
				excl += e->fe_length;
			start = e->fe_logical + e->fe_length;
			if (e->fe_flags & FIEMAP_EXTENT_LAST)
				last = 1;
		}
		if (last || fm->fm_mapped_extents < EXTENTS_PER_CALL)
			break;
	}
	close(fd);
	t->exclusive += excl;
	t->shared += shared;
}

// walk measures the entry name (relative to parent) and, for a directory,
// everything below it on the same filesystem. path holds the entry's path
// for messages (pathlen bytes); it is extended in place and restored.
static void walk(int parent, const char *name, char *path, size_t pathlen, dev_t root_dev, int depth, struct totals *t)
{
	struct stat st;
	if (fstatat(parent, name, &st, AT_SYMLINK_NOFOLLOW) < 0) {
		if (!vanished(errno) || depth == 0)
			fail(path, "stat", errno);
		return;
	}
	if (depth > 0 && st.st_dev != root_dev)
		return; // another filesystem mounted below the root is not ours
	if (!S_ISDIR(st.st_mode) && st.st_nlink > 1 && !seen_insert(st.st_dev, st.st_ino))
		return; // counted through another link already
	if (S_ISREG(st.st_mode)) {
		measure_file(parent, name, path, &st, t);
		return;
	}
	t->apparent += (uint64_t)st.st_size;
	t->exclusive += allocated(&st);
	if (!S_ISDIR(st.st_mode))
		return;
	if (depth >= MAX_DEPTH) {
		fail(path, "walk", ELOOP);
		return;
	}
	int fd = openat(parent, name, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC);
	if (fd < 0) {
		if (!vanished(errno))
			fail(path, "open directory", errno);
		return;
	}
	DIR *d = fdopendir(fd);
	if (!d) {
		fail(path, "fdopendir", errno);
		close(fd);
		return;
	}
	for (;;) {
		errno = 0;
		struct dirent *de = readdir(d);
		if (!de) {
			if (errno != 0 && !vanished(errno))
				fail(path, "readdir", errno);
			break;
		}
		const char *n = de->d_name;
		if (n[0] == '.' && (n[1] == '\0' || (n[1] == '.' && n[2] == '\0')))
			continue;
		size_t nlen = strlen(n);
		if (pathlen + 1 + nlen + 1 > PATH_MAX) {
			fail(path, n, ENAMETOOLONG);
			continue;
		}
		path[pathlen] = '/';
		memcpy(path + pathlen + 1, n, nlen + 1);
		walk(dirfd(d), n, path, pathlen + 1 + nlen, root_dev, depth + 1, t);
		path[pathlen] = '\0';
	}
	closedir(d);
}

// measure prints the totals line for one PATH. It returns 0, or -1 when PATH
// itself cannot be read.
static int measure(const char *root)
{
	struct stat st;
	if (lstat(root, &st) < 0) {
		fail(root, "stat", errno);
		return -1;
	}
	static char path[PATH_MAX];
	size_t len = strlen(root);
	if (len + 1 > sizeof path) {
		fail(root, "path", ENAMETOOLONG);
		return -1;
	}
	memcpy(path, root, len + 1);
	struct totals t = {0, 0, 0};
	walk(AT_FDCWD, root, path, len, st.st_dev, 0, &t);
	printf("%" PRIu64 "\t%" PRIu64 "\t%" PRIu64 "\t%s\n", t.exclusive, t.shared, t.apparent, root);
	return 0;
}

// ---- XFS copy-on-write extent size hint

static int set_cowextsize(const char *arg, const char *dir)
{
	char *end;
	errno = 0;
	unsigned long long bytes = strtoull(arg, &end, 10);
	if (errno != 0 || end == arg || *end != '\0' || bytes > UINT32_MAX) {
		fprintf(stderr, "%s: -c: %s is not a byte count\n", prog, arg);
		return 2;
	}
	int fd = open(dir, O_RDONLY | O_DIRECTORY | O_CLOEXEC);
	if (fd < 0) {
		fail(dir, "open directory", errno);
		return 2;
	}
	struct fsxattr fsx;
	memset(&fsx, 0, sizeof fsx);
	if (ioctl(fd, FS_IOC_FSGETXATTR, &fsx) < 0) {
		fail(dir, "FS_IOC_FSGETXATTR", errno);
		close(fd);
		return 1;
	}
	if (bytes == 0) {
		fsx.fsx_xflags &= ~(uint32_t)FS_XFLAG_COWEXTSIZE;
		fsx.fsx_cowextsize = 0;
	} else {
		fsx.fsx_xflags |= FS_XFLAG_COWEXTSIZE;
		fsx.fsx_cowextsize = (uint32_t)bytes;
	}
	if (ioctl(fd, FS_IOC_FSSETXATTR, &fsx) < 0) {
		fail(dir, "FS_IOC_FSSETXATTR", errno);
		close(fd);
		return 1;
	}
	memset(&fsx, 0, sizeof fsx);
	if (ioctl(fd, FS_IOC_FSGETXATTR, &fsx) < 0) {
		fail(dir, "FS_IOC_FSGETXATTR", errno);
		close(fd);
		return 1;
	}
	close(fd);
	unsigned got = (fsx.fsx_xflags & FS_XFLAG_COWEXTSIZE) ? fsx.fsx_cowextsize : 0;
	printf("cowextsize=%u\t%s\n", got, dir);
	return 0;
}

static int usage(void)
{
	fprintf(stderr,
		"usage: %s PATH...        print <exclusive>\\t<shared>\\t<apparent>\\t<PATH> bytes per PATH\n"
		"       %s -c BYTES DIR   set the XFS copy-on-write extent size hint of DIR (0 clears it)\n"
		"       %s -V             print the version\n",
		prog, prog, prog);
	return 2;
}

int main(int argc, char **argv)
{
	if (argc < 2)
		return usage();
	if (strcmp(argv[1], "-V") == 0 && argc == 2) {
		printf("pgoverlay-du %s\n", PGOVERLAY_DU_VERSION);
		return 0;
	}
	if (strcmp(argv[1], "-c") == 0) {
		if (argc != 4)
			return usage();
		return set_cowextsize(argv[2], argv[3]);
	}
	int first = 1;
	if (strcmp(argv[1], "--") == 0)
		first = 2;
	else if (argv[1][0] == '-')
		return usage();
	if (first >= argc)
		return usage();
	fm = malloc(sizeof *fm + EXTENTS_PER_CALL * sizeof(struct fiemap_extent));
	if (!fm) {
		fprintf(stderr, "%s: out of memory\n", prog);
		return 2;
	}
	int fatal = 0;
	for (int i = first; i < argc; i++)
		if (measure(argv[i]) < 0)
			fatal = 1;
	if (fflush(stdout) != 0 || ferror(stdout)) {
		fprintf(stderr, "%s: write error\n", prog);
		return 2;
	}
	if (fatal)
		return 2;
	return errors ? 1 : 0;
}
