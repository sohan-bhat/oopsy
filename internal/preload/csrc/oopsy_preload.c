/*
 * Oopsy's Linux preload library.
 *
 * On ext4 and other disks without copy on write, Oopsy's project mirror
 * holds hard links, which share each file with the original. A program that
 * opens a file for writing and changes it in place changes the mirror's link
 * too, so the old version would be gone. Loaded with LD_PRELOAD, this library
 * copies a mirrored file right before a program first opens it for writing,
 * into ~/.oops/prewrite/<hash of the project root>/<hash of its path>,
 * keeping its size and modification time. Only the first write of a command
 * is saved: Oopsy clears the folder each time it syncs the project. Oopsy's next sync picks the copy up.
 *
 * It does nothing outside the folders listed in ~/.oops/prewrite/roots,
 * never changes what the program sees, and gives up quietly on any error.
 * Statically linked programs (most Go binaries, for example) do not load
 * preload libraries, so they are not covered.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sendfile.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <unistd.h>

#define MAX_ROOTS 64

/* snprintf that reports whether everything fit. */
#define FITS(buf, ...) (snprintf((buf), sizeof(buf), __VA_ARGS__) < (int)sizeof(buf))

static char store[PATH_MAX];
static char roots[MAX_ROOTS][PATH_MAX];
static int nroots = -1;
static long long max_bytes = 512LL << 20;
static pthread_once_t once = PTHREAD_ONCE_INIT;
static __thread int busy;

/* Raw open, so reading our own config never recurses into the hooks. */
static int raw_open(const char *p, int flags, mode_t mode) {
    return (int)syscall(SYS_openat, AT_FDCWD, p, flags, mode);
}

static void load(void) {
    const char *home = getenv("OOPS_HOME");
    char base[PATH_MAX];
    nroots = 0;
    if (home && *home) {
        if (!FITS(base, "%s", home)) return;
    } else {
        const char *h = getenv("HOME");
        if (!h || !*h) return;
        if (!FITS(base, "%s/.oops", h)) return;
    }
    if (!FITS(store, "%s/prewrite", base)) return;
    const char *mx = getenv("OOPSY_PREWRITE_MAX");
    if (mx && *mx) max_bytes = atoll(mx);
    char rp[PATH_MAX];
    if (!FITS(rp, "%s/roots", store)) return;
    int fd = raw_open(rp, O_RDONLY | O_CLOEXEC, 0);
    if (fd < 0) return;
    char buf[MAX_ROOTS * 256];
    ssize_t n = read(fd, buf, sizeof buf - 1);
    close(fd);
    if (n <= 0) return;
    buf[n] = 0;
    char *save = NULL;
    for (char *line = strtok_r(buf, "\n", &save); line && nroots < MAX_ROOTS; line = strtok_r(NULL, "\n", &save)) {
        if (*line == '/' && strlen(line) < PATH_MAX) memcpy(roots[nroots++], line, strlen(line) + 1);
    }
}

static const char *under_root(const char *p) {
    for (int i = 0; i < nroots; i++) {
        size_t l = strlen(roots[i]);
        if (strncmp(p, roots[i], l) == 0 && (p[l] == '/' || p[l] == 0)) return roots[i];
    }
    return NULL;
}

/* FNV 1a, 64 bit, the same as snap.PrewriteName in Go. */
static uint64_t fnv(const char *s) {
    uint64_t h = 1469598103934665603ULL;
    for (; *s; s++) { h ^= (unsigned char)*s; h *= 1099511628211ULL; }
    return h;
}

/* Absolute path of p as opened relative to dirfd, without resolving the
 * final component (it must be the file itself, not where a link points). */
static int absolute(int dirfd, const char *p, char *out) {
    if (p[0] == '/') return snprintf(out, PATH_MAX, "%s", p) < PATH_MAX ? 0 : -1;
    char dir[PATH_MAX];
    if (dirfd == AT_FDCWD) {
        if (!getcwd(dir, sizeof dir)) return -1;
    } else {
        char link[64];
        snprintf(link, sizeof link, "/proc/self/fd/%d", dirfd);
        ssize_t n = readlink(link, dir, sizeof dir - 1);
        if (n <= 0) return -1;
        dir[n] = 0;
    }
    if (snprintf(out, PATH_MAX, "%s/%s", dir, p) >= PATH_MAX) return -1;
    return 0;
}

static void save_copy_inner(int dirfd, const char *p);

static void save_copy(int dirfd, const char *p) {
    if (!p || busy) return;
    int saved_errno = errno;
    save_copy_inner(dirfd, p);
    errno = saved_errno;
}

static void save_copy_inner(int dirfd, const char *p) {
    pthread_once(&once, load);
    if (nroots <= 0) return;
    char abs[PATH_MAX];
    if (absolute(dirfd, p, abs) != 0) return;
    const char *root = under_root(abs);
    if (!root) return;
    struct stat st;
    if (lstat(abs, &st) != 0 || !S_ISREG(st.st_mode) || st.st_size > max_bytes) return;
    if (st.st_nlink < 2) return; /* not hard linked into a mirror */
    busy = 1;
    char dir[PATH_MAX], dst[PATH_MAX], tmp[PATH_MAX];
    if (!FITS(dir, "%s/%016llx", store, (unsigned long long)fnv(root))) goto out;
    mkdir(dir, 0700);
    if (!FITS(dst, "%s/%016llx", dir, (unsigned long long)fnv(abs))) goto out;
    struct stat old;
    if (lstat(dst, &old) == 0) goto out; /* the first write of this command was already saved */
    if (!FITS(tmp, "%s.%d.tmp", dst, (int)getpid())) goto out;
    int in = raw_open(abs, O_RDONLY | O_CLOEXEC, 0);
    if (in < 0) goto out;
    int o = raw_open(tmp, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
    if (o < 0) { close(in); goto out; }
    off_t left = st.st_size;
    int ok = 1;
    while (left > 0) {
        ssize_t n = sendfile(o, in, NULL, left > (1 << 30) ? (1 << 30) : (size_t)left);
        if (n <= 0) { ok = 0; break; }
        left -= n;
    }
    close(in);
    struct timespec ts[2] = {st.st_atim, st.st_mtim};
    if (ok) ok = futimens(o, ts) == 0;
    if (close(o) != 0) ok = 0;
    if (!ok || rename(tmp, dst) != 0) unlink(tmp);
out:
    busy = 0;
}

static int writes(int flags) { return (flags & O_ACCMODE) != O_RDONLY || (flags & O_TRUNC); }

static int mode_writes(const char *m) { return m && (strchr(m, 'w') || strchr(m, 'a') || strchr(m, '+')); }

#define NEXT(name) static __typeof__(name) *next_##name; if (!next_##name) next_##name = dlsym(RTLD_NEXT, #name)

int open(const char *p, int flags, ...) {
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) { va_list a; va_start(a, flags); mode = va_arg(a, mode_t); va_end(a); }
    if (writes(flags)) save_copy(AT_FDCWD, p);
    NEXT(open);
    return next_open(p, flags, mode);
}

int open64(const char *p, int flags, ...) {
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) { va_list a; va_start(a, flags); mode = va_arg(a, mode_t); va_end(a); }
    if (writes(flags)) save_copy(AT_FDCWD, p);
    NEXT(open64);
    return next_open64(p, flags, mode);
}

int openat(int d, const char *p, int flags, ...) {
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) { va_list a; va_start(a, flags); mode = va_arg(a, mode_t); va_end(a); }
    if (writes(flags)) save_copy(d, p);
    NEXT(openat);
    return next_openat(d, p, flags, mode);
}

int openat64(int d, const char *p, int flags, ...) {
    mode_t mode = 0;
    if (flags & (O_CREAT | O_TMPFILE)) { va_list a; va_start(a, flags); mode = va_arg(a, mode_t); va_end(a); }
    if (writes(flags)) save_copy(d, p);
    NEXT(openat64);
    return next_openat64(d, p, flags, mode);
}

int creat(const char *p, mode_t mode) {
    save_copy(AT_FDCWD, p);
    NEXT(creat);
    return next_creat(p, mode);
}

int creat64(const char *p, mode_t mode) {
    save_copy(AT_FDCWD, p);
    NEXT(creat64);
    return next_creat64(p, mode);
}

FILE *fopen(const char *p, const char *m) {
    if (mode_writes(m)) save_copy(AT_FDCWD, p);
    NEXT(fopen);
    return next_fopen(p, m);
}

FILE *fopen64(const char *p, const char *m) {
    if (mode_writes(m)) save_copy(AT_FDCWD, p);
    NEXT(fopen64);
    return next_fopen64(p, m);
}

FILE *freopen(const char *p, const char *m, FILE *f) {
    if (mode_writes(m)) save_copy(AT_FDCWD, p);
    NEXT(freopen);
    return next_freopen(p, m, f);
}

int truncate(const char *p, off_t len) {
    save_copy(AT_FDCWD, p);
    NEXT(truncate);
    return next_truncate(p, len);
}

int truncate64(const char *p, off64_t len) {
    save_copy(AT_FDCWD, p);
    NEXT(truncate64);
    return next_truncate64(p, len);
}
