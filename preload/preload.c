/* libflatpark-spawn.so: loaded with LD_PRELOAD by the consumer's wrapper
 * (enable.sh) only after the app was found to hold
 * --talk-name=org.freedesktop.Flatpak; without that nothing loads it.
 *
 * - exec family and posix_spawn family: route.c decides; a host exec
 *   becomes an exec of the flatpark-spawn command, which runs it through
 *   HostCommand.
 * - bash's command_not_found_handle, exported as an environment function,
 *   covers commands bash looks up itself (`sh -c name`).
 * - socket/socketpair: a family the sandbox's seccomp filter refuses
 *   (AF_VSOCK, ...) is created on the host and the fd passed back.
 *
 * Not hooked on purpose: fexecve/execveat (an fd of a sandbox file has no
 * host meaning), access/stat/open (no host OS fallback in this version). */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <spawn.h>
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "names.h"
#include "route.h"

#define EXPORT __attribute__((visibility("default")))

extern char **environ;

typedef int (*execve_fn)(const char *, char *const[], char *const[]);
typedef int (*execvp_fn)(const char *, char *const[]);
typedef int (*execvpe_fn)(const char *, char *const[], char *const[]);
typedef int (*spawn_fn)(pid_t *, const char *, const posix_spawn_file_actions_t *,
                        const posix_spawnattr_t *, char *const[], char *const[]);
typedef int (*socket_fn)(int, int, int);
typedef int (*socketpair_fn)(int, int, int, int[2]);

static execve_fn real_execve;
static execvp_fn real_execvp;
static execvpe_fn real_execvpe;
static spawn_fn real_posix_spawn;
static spawn_fn real_posix_spawnp;
static socket_fn real_socket;
static socketpair_fn real_socketpair;

/* dlsym returns void *; the cast to a function pointer is the POSIX idiom. */
#define RESOLVE(var, name) \
    do { if (!var) *(void **)(&var) = dlsym(RTLD_NEXT, name); } while (0)

static void resolve(void) {
    RESOLVE(real_execve, "execve");
    RESOLVE(real_execvp, "execvp");
    RESOLVE(real_execvpe, "execvpe");
    RESOLVE(real_posix_spawn, "posix_spawn");
    RESOLVE(real_posix_spawnp, "posix_spawnp");
    RESOLVE(real_socket, "socket");
    RESOLVE(real_socketpair, "socketpair");
}

/* A variable as the new program will see it: from the envp passed to the
 * exec (bash passes `VAR=x cmd` only there), else from our environment.
 * Scans the array itself rather than calling getenv, which POSIX does not
 * count as async-signal-safe (the exec hooks run in fork/vfork children). */
static const char *env_get(char *const envp[], const char *name) {
    char *const *e = envp ? envp : environ;
    size_t n = strlen(name);
    for (size_t i = 0; e && e[i]; i++)
        if (strncmp(e[i], name, n) == 0 && e[i][n] == '=') return e[i] + n + 1;
    return NULL;
}

static int active_in(char *const envp[]) {
    const char *v = env_get(envp, FPS_ENV("ENABLE"));
    return v && v[0] == '1' && v[1] == '\0';
}

static int active(void) { return active_in(NULL); }

static int debug(void) {
    const char *v = env_get(NULL, FPS_ENV("DEBUG"));
    return v && *v && strcmp(v, "0") != 0;
}

/* stderr without stdio: fprintf takes the FILE lock, which another thread
 * may have held when this process forked. */
static void say(const char *const parts[], size_t n) {
    char buf[1024];
    size_t pos = 0;
    for (size_t i = 0; i < n; i++)
        if (fps_append(buf, sizeof(buf) - 1, &pos, parts[i], strlen(parts[i])) < 0) break;
    buf[pos++] = '\n';
    ssize_t w;
    for (size_t off = 0; off < pos; off += (size_t)w)
        if ((w = write(2, buf + off, pos - off)) <= 0) break;
}

/* Memory, fork and vfork: the exec hooks run in the child of a fork in a
 * multithreaded process (Chromium, node-pty, libuv) and in vfork children
 * (Python's subprocess, posix_spawn's own child), which share the parent's
 * memory and whose parent may hold locks or library state of other threads
 * (zypak's preload keeps sd-bus/sd-event state). So the exec path is
 * async-signal-safe: no malloc, no stdio, no getenv, no locks. Argument and
 * environment arrays are built on the stack up to STACK_BYTES, beyond that
 * in an anonymous mmap (a system call, no allocator state). */
#define STACK_BYTES (64 * 1024)

static void *map_scratch(size_t bytes) {
    void *p = mmap(NULL, bytes, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
    return p == MAP_FAILED ? NULL : p;
}

static size_t count(char *const v[]) {
    size_t n = 0;
    while (v && v[n]) n++;
    return n;
}

/* The environment of the flatpark-spawn command (a forward or a lookup):
 * it is a Go program, and Go's runtime starts measurably faster with one P
 * (GOMAXPROCS=1, about 0.2 ms of the ~4.5 ms a forward costs). Added only
 * when the caller has no GOMAXPROCS of its own, together with a marker by
 * which the command removes it again before anything reaches the host or a
 * local fallback. A local exec gets its envp exactly as the caller passed
 * it (DESIGN.md, 否决的做法: 补回 preload). */
struct env_plan {
    size_t n; /* entries in src */
    int gomax;
};

static void plan_env(char *const src[], struct env_plan *p) {
    p->gomax = 1;
    size_t n = 0;
    for (; src[n]; n++)
        if (strncmp(src[n], "GOMAXPROCS=", 11) == 0) p->gomax = 0;
    p->n = n;
}

/* Writes the new environment into out (room for n + 3 entries); returns
 * src itself when nothing changes. */
static char *const *build_env(char *const src[], const struct env_plan *p, char **out) {
    if (!p->gomax) return src;
    memcpy(out, src, p->n * sizeof(*out));
    out[p->n] = (char *)"GOMAXPROCS=1";
    out[p->n + 1] = (char *)FPS_ENV("GOMAXPROCS") "=1";
    out[p->n + 2] = NULL;
    return out;
}

/* bash runs command_not_found_handle when its own PATH search fails, which
 * never reaches exec. Exporting it as a function reaches every bash in the
 * process tree, `sh -c` included (bash in POSIX mode; /bin/sh is bash in
 * the freedesktop runtimes). An app's own handler is left alone. */
__attribute__((constructor)) static void init(void) {
    resolve();
    if (!active()) return;
    setenv("BASH_FUNC_command_not_found_handle%%",
           "() { " FPS_HELPER " " FPS_MODE_SHELL " \"${0##*/}\" \"$@\"\n}", 0);
}

/* 1 when the app was granted the host's /tmp (`--filesystem=/tmp`).
 * `--filesystem=host` does not share /tmp. No stdio and no malloc: this
 * runs in vfork children (see the note at forward_exec). */
static int tmp_shared(void) {
    static int cached = -1;
    if (cached >= 0) return cached;
    cached = 0;
    char buf[16384];
    int fd = open("/.flatpak-info", O_RDONLY | O_CLOEXEC);
    if (fd < 0) return cached;
    ssize_t n, len = 0;
    while (len < (ssize_t)sizeof(buf) - 1 &&
           (n = read(fd, buf + len, sizeof(buf) - 1 - (size_t)len)) > 0)
        len += n;
    close(fd);
    buf[len] = '\0';
    char *ctx = strstr(buf, "\n[Context]\n");
    if (!ctx) return cached;
    char *end = strstr(ctx + 1, "\n[");
    char *line = strstr(ctx, "\nfilesystems=");
    if (!line || (end && line > end)) return cached;
    line += strlen("\nfilesystems=");
    char *eol = strchr(line, '\n');
    for (char *e = line; e && *e && (!eol || e < eol);) {
        size_t elen = strcspn(e, ";\n");
        if ((elen == 4 && strncmp(e, "/tmp", 4) == 0) ||
            (elen > 4 && strncmp(e, "/tmp:", 5) == 0))
            cached = 1;
        e += elen;
        if (*e == ';') e++;
        else break;
    }
    return cached;
}

/* Runs fn(argv', envp', data) for the flatpark-spawn command, with the
 * arguments and environment built without the allocator (see STACK_BYTES).
 * fwd_target != NULL: argv' is the forwarding command line
 * [flatpark-spawn, --x-exec, target, argv0, argv[1..]]; NULL: argv itself.
 * When the scratch memory cannot be had it fails with ENOMEM (a forward
 * then goes on locally). */
typedef int (*launch_fn)(char *const argv[], char *const envp[], void *data);

static size_t fill_argv(char **out, const char *target, char *const argv[]) {
    size_t argc = count(argv);
    out[0] = (char *)FPS_NAME;
    out[1] = (char *)FPS_MODE_EXEC;
    out[2] = (char *)target;
    out[3] = argc > 0 ? argv[0] : (char *)target;
    for (size_t i = 1; i < argc; i++) out[i + 3] = argv[i];
    size_t end = argc > 0 ? argc + 3 : 4;
    out[end] = NULL;
    return end + 1;
}

static int launch_in(void **mem, const char *fwd_target, char *const argv[], char *const src[],
                     const struct env_plan *plan, launch_fn fn, void *data) {
    char **av = (char **)mem;
    size_t used = 0;
    char *const *a = argv;
    if (fwd_target) {
        used = fill_argv(av, fwd_target, argv);
        a = av;
    }
    return fn(a, build_env(src, plan, av + used), data);
}

static int launch(const char *fwd_target, char *const argv[], char *const envp[],
                  launch_fn fn, void *data) {
    char *const *src = envp ? envp : environ;
    struct env_plan plan;
    plan_env(src, &plan);
    size_t words = fwd_target ? count(argv) + 5 : 0;
    if (plan.gomax) words += plan.n + 3;
    if (words == 0) return fn(argv, src, data);
    size_t bytes = words * sizeof(void *);
    if (bytes <= STACK_BYTES) {
        void *mem[words];
        return launch_in(mem, fwd_target, argv, src, &plan, fn, data);
    }
    void **mem = map_scratch(bytes);
    if (!mem) {
        errno = ENOMEM;
        return -1;
    }
    int rc = launch_in(mem, fwd_target, argv, src, &plan, fn, data);
    int saved = errno;
    munmap(mem, bytes);
    errno = saved;
    return rc;
}

struct spawn_args {
    spawn_fn real;
    const char *file;
    pid_t *pid;
    const posix_spawn_file_actions_t *fa;
    const posix_spawnattr_t *attr;
};

static int do_spawn(char *const argv[], char *const envp[], void *data) {
    struct spawn_args *a = data;
    return a->real(a->pid, a->file, a->fa, a->attr, argv, envp);
}

/* No file actions: posix_spawn_file_actions_addopen copies the path with
 * malloc. The command points the host side's stdin and stdout at /dev/null
 * itself in this mode. */
static int lookup_on_host(const struct fps_ctx *ctx, const char *name) {
    (void)ctx;
    char *argv[] = {(char *)FPS_NAME, (char *)FPS_MODE_LOOKUP, (char *)name, NULL};
    pid_t pid;
    struct spawn_args a = {real_posix_spawn, FPS_HELPER, &pid, NULL, NULL};
    if (launch(NULL, argv, environ, do_spawn, &a) != 0) return FPS_LOOKUP_ERROR;
    int status;
    while (waitpid(pid, &status, 0) < 0)
        if (errno != EINTR) return FPS_LOOKUP_ERROR;
    if (!WIFEXITED(status)) return FPS_LOOKUP_ERROR;
    switch (WEXITSTATUS(status)) {
    case 0: return FPS_LOOKUP_HOST;
    case 1: return FPS_LOOKUP_NONE;
    default: return FPS_LOOKUP_ERROR;
    }
}

struct ctx_bufs {
    char host[4096];
    char none[4096];
    char cwd[4096];
};

/* getcwd as the system call: glibc's getcwd falls back to an opendir walk
 * (malloc) for long or unreachable paths. */
static const char *physical_cwd(char *buf, size_t size) {
    long n = syscall(SYS_getcwd, buf, size);
    return n > 0 && buf[0] == '/' ? buf : NULL;
}

static int cache_dir(char *out, size_t cap, const char *cache, const char *key, const char *leaf) {
    size_t pos = 0;
    const char *parts[] = {cache, "/" FPS_CACHE_CWD_DIR "/", key, "/", leaf};
    for (size_t i = 0; i < sizeof(parts) / sizeof(*parts); i++)
        if (fps_append(out, cap, &pos, parts[i], strlen(parts[i])) < 0) return -1;
    return 0;
}

static void make_ctx(struct fps_ctx *ctx, struct ctx_bufs *b, char *const envp[]) {
    memset(ctx, 0, sizeof(*ctx));
    ctx->path = env_get(envp, "PATH");
    ctx->local_list = env_get(envp, FPS_ENV("LOCAL"));
    ctx->shim_dir = env_get(envp, FPS_ENV("SHIMS"));
    ctx->cwd = physical_cwd(b->cwd, sizeof(b->cwd));
    /* The cache is per working directory: what the host has depends on it
     * (mise, direnv). */
    const char *cache = env_get(envp, FPS_ENV("CACHE"));
    if (cache && cache[0] == '/' && ctx->cwd) {
        char key[17];
        fps_cwd_key(ctx->cwd, key);
        if (cache_dir(b->host, sizeof(b->host), cache, key, FPS_CACHE_HOST_DIR) == 0 &&
            cache_dir(b->none, sizeof(b->none), cache, key, FPS_CACHE_NONE_DIR) == 0) {
            ctx->cache_host = b->host;
            ctx->cache_none = b->none;
        }
    }
    ctx->tmp_shared = tmp_shared();
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ctx->now = (long long)ts.tv_sec;
    ctx->local_exists = fps_path_has;
    ctx->host_lookup = lookup_on_host;
}

/* FPS_HOST and the target, or FPS_LOCAL. */
static enum fps_route decide(const char *file, char *const argv[], char *const envp[],
                             int path_search, const char **target) {
    *target = NULL;
    if (!active_in(envp)) return FPS_LOCAL;
    struct fps_ctx ctx;
    struct ctx_bufs bufs;
    make_ctx(&ctx, &bufs, envp);
    enum fps_route r = fps_route_exec(&ctx, file, argv, path_search, target);
    if (debug()) {
        const char *parts[] = {FPS_NAME ": ", file ? file : "(null)",
                               r == FPS_HOST ? " -> host" : " -> sandbox"};
        say(parts, 3);
    }
    return r;
}

static int do_helper_execve(char *const argv[], char *const envp[], void *data) {
    (void)data;
    return real_execve(FPS_HELPER, argv, envp);
}

/* Returns only when the command could not be started; the caller then
 * goes on with the original exec. */
static void forward_exec(const char *target, char *const argv[], char *const envp[]) {
    int saved = errno;
    launch(target, argv, envp, do_helper_execve, NULL);
    errno = saved;
}

EXPORT int execve(const char *file, char *const argv[], char *const envp[]) {
    resolve();
    const char *target;
    if (decide(file, argv, envp, 0, &target) == FPS_HOST) forward_exec(target, argv, envp);
    return real_execve(file, argv, envp);
}

EXPORT int execv(const char *file, char *const argv[]) {
    return execve(file, argv, environ);
}

EXPORT int execvp(const char *file, char *const argv[]) {
    resolve();
    const char *target;
    if (decide(file, argv, NULL, 1, &target) == FPS_HOST) forward_exec(target, argv, environ);
    return real_execvp(file, argv);
}

EXPORT int execvpe(const char *file, char *const argv[], char *const envp[]) {
    resolve();
    const char *target;
    if (decide(file, argv, envp, 1, &target) == FPS_HOST) forward_exec(target, argv, envp);
    return real_execvpe(file, argv, envp);
}

/* execl, execle and execlp: the variadic arguments are counted first and
 * put in an array on the stack (or an mmap): execl and execle are
 * async-signal-safe in POSIX, so they may not allocate either. */
enum { VIA_EXECVE, VIA_EXECVP };

static int exec_va(int via, int with_envp, const char *file, const char *arg0, va_list ap) {
    size_t n = 0;
    if (arg0) {
        va_list c;
        va_copy(c, ap);
        for (n = 1; va_arg(c, const char *); n++) {}
        va_end(c);
    }
    size_t bytes = (n + 1) * sizeof(char *);
    int small = bytes <= STACK_BYTES;
    char *stackv[small ? n + 1 : 1];
    char **argv = small ? stackv : map_scratch(bytes);
    if (!argv) { errno = ENOMEM; return -1; }
    if (arg0) {
        argv[0] = (char *)arg0;
        for (size_t i = 1; i < n; i++) argv[i] = va_arg(ap, char *);
        (void)va_arg(ap, char *); /* the terminating NULL */
    }
    argv[n] = NULL;
    char *const *envp = with_envp ? va_arg(ap, char *const *) : environ;
    int r = via == VIA_EXECVP ? execvp(file, argv) : execve(file, argv, envp);
    int saved = errno;
    if (!small) munmap(argv, bytes);
    errno = saved;
    return r;
}

EXPORT int execl(const char *file, const char *arg0, ...) {
    va_list ap;
    va_start(ap, arg0);
    int r = exec_va(VIA_EXECVE, 0, file, arg0, ap);
    va_end(ap);
    return r;
}

EXPORT int execlp(const char *file, const char *arg0, ...) {
    va_list ap;
    va_start(ap, arg0);
    int r = exec_va(VIA_EXECVP, 0, file, arg0, ap);
    va_end(ap);
    return r;
}

EXPORT int execle(const char *file, const char *arg0, ...) {
    va_list ap;
    va_start(ap, arg0);
    int r = exec_va(VIA_EXECVE, 1, file, arg0, ap);
    va_end(ap);
    return r;
}

static int spawn_routed(spawn_fn real, int path_search, pid_t *pid, const char *file,
                        const posix_spawn_file_actions_t *fa, const posix_spawnattr_t *attr,
                        char *const argv[], char *const envp[]) {
    resolve();
    const char *target;
    if (decide(file, argv, envp, path_search, &target) == FPS_HOST) {
        struct spawn_args h = {real_posix_spawn, FPS_HELPER, pid, fa, attr};
        if (launch(target, argv, envp, do_spawn, &h) == 0) return 0;
    }
    return real(pid, file, fa, attr, argv, envp);
}

EXPORT int posix_spawn(pid_t *pid, const char *path, const posix_spawn_file_actions_t *fa,
                       const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    resolve();
    return spawn_routed(real_posix_spawn, 0, pid, path, fa, attr, argv, envp);
}

EXPORT int posix_spawnp(pid_t *pid, const char *file, const posix_spawn_file_actions_t *fa,
                        const posix_spawnattr_t *attr, char *const argv[], char *const envp[]) {
    resolve();
    return spawn_routed(real_posix_spawnp, 1, pid, file, fa, attr, argv, envp);
}

/* ---- socket delegation ---- */

static int recv_fds(int control, int *fds, size_t count, int cloexec) {
    char data;
    union {
        char buf[CMSG_SPACE(sizeof(int) * 2)];
        struct cmsghdr align;
    } u;
    struct iovec iov = {.iov_base = &data, .iov_len = 1};
    struct msghdr msg;
    memset(&msg, 0, sizeof(msg));
    msg.msg_iov = &iov;
    msg.msg_iovlen = 1;
    msg.msg_control = u.buf;
    msg.msg_controllen = sizeof(u.buf);
    ssize_t n;
    while ((n = recvmsg(control, &msg, cloexec ? MSG_CMSG_CLOEXEC : 0)) < 0 && errno == EINTR) {}
    if (n <= 0) return -1;
    struct cmsghdr *c = CMSG_FIRSTHDR(&msg);
    if (!c || c->cmsg_level != SOL_SOCKET || c->cmsg_type != SCM_RIGHTS ||
        c->cmsg_len != CMSG_LEN(sizeof(int) * count))
        return -1;
    memcpy(fds, CMSG_DATA(c), sizeof(int) * count);
    return 0;
}

static void decimal(long long v, char out[24]) {
    char tmp[24];
    size_t n = 0;
    unsigned long long u = v < 0 ? 0ULL - (unsigned long long)v : (unsigned long long)v;
    do { tmp[n++] = (char)('0' + u % 10); u /= 10; } while (u);
    size_t k = 0;
    if (v < 0) out[k++] = '-';
    while (n) out[k++] = tmp[--n];
    out[k] = '\0';
}

/* Creates the socket(s) on the host: the command runs itself there through
 * HostCommand with the control socket as fd 3, and sends the result back
 * with SCM_RIGHTS. One short-lived helper per call; no resident broker. */
static int delegate_socket(int domain, int type, int protocol, int *fds, size_t count) {
    int ctl[2];
    if (real_socketpair(AF_UNIX, SOCK_SEQPACKET | SOCK_CLOEXEC, 0, ctl) < 0) return -1;
    if (ctl[1] == 3) { /* dup2(3, 3) would keep FD_CLOEXEC */
        int moved = fcntl(ctl[1], F_DUPFD_CLOEXEC, 4);
        close(ctl[1]);
        ctl[1] = moved;
        if (moved < 0) { close(ctl[0]); return -1; }
    }
    char d[24], t[24], p[24], c[24];
    decimal(domain, d);
    decimal(type, t);
    decimal(protocol, p);
    decimal((long long)count, c);
    char *argv[] = {(char *)FPS_NAME, (char *)FPS_MODE_SOCKET, d, t, p, c, NULL};
    posix_spawn_file_actions_t fa;
    pid_t pid;
    int rc = posix_spawn_file_actions_init(&fa);
    if (rc == 0) {
        posix_spawn_file_actions_adddup2(&fa, ctl[1], 3);
        rc = real_posix_spawn(&pid, FPS_HELPER, &fa, NULL, argv, environ);
        posix_spawn_file_actions_destroy(&fa);
    }
    close(ctl[1]);
    if (rc != 0) { close(ctl[0]); return -1; }
    int result = recv_fds(ctl[0], fds, count, (type & SOCK_CLOEXEC) != 0);
    close(ctl[0]);
    int status;
    while (waitpid(pid, &status, 0) < 0 && errno == EINTR) {}
    return result;
}

EXPORT int socket(int domain, int type, int protocol) {
    resolve();
    int fd = real_socket(domain, type, protocol);
    if (fd >= 0 || errno != EAFNOSUPPORT || !active()) return fd;
    int out;
    if (delegate_socket(domain, type, protocol, &out, 1) < 0) {
        errno = EAFNOSUPPORT;
        return -1;
    }
    return out;
}

EXPORT int socketpair(int domain, int type, int protocol, int sv[2]) {
    resolve();
    int r = real_socketpair(domain, type, protocol, sv);
    if (r == 0 || errno != EAFNOSUPPORT || !active()) return r;
    if (delegate_socket(domain, type, protocol, sv, 2) < 0) {
        errno = EAFNOSUPPORT;
        return -1;
    }
    return 0;
}
