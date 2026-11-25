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
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/stat.h>
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
 * exec (bash passes `VAR=x cmd` only there), else from our environment. */
static const char *env_get(char *const envp[], const char *name) {
    if (!envp || envp == environ) return getenv(name);
    size_t n = strlen(name);
    for (size_t i = 0; envp[i]; i++)
        if (strncmp(envp[i], name, n) == 0 && envp[i][n] == '=') return envp[i] + n + 1;
    return NULL;
}

static int active_in(char *const envp[]) {
    const char *v = env_get(envp, FPS_ENV("ENABLE"));
    return v && v[0] == '1' && v[1] == '\0';
}

static int active(void) { return active_in(NULL); }

static int debug(void) {
    const char *v = getenv(FPS_ENV("DEBUG"));
    return v && *v && strcmp(v, "0") != 0;
}

/* Memory and vfork: the exec hooks also run in vfork children (Python's
 * subprocess, posix_spawn's own child), which share the parent's memory and
 * whose parent may hold locks or library state of other threads (zypak's
 * preload keeps sd-bus/sd-event state). So the exec path does not allocate:
 * argument and environment arrays are built on the stack, up to STACK_ARGS
 * entries, and only beyond that fall back to malloc. */
#define STACK_ARGS 4096

static size_t count(char *const v[]) {
    size_t n = 0;
    while (v && v[n]) n++;
    return n;
}

/* The command is a Go program, and Go's runtime starts measurably faster
 * with one P (GOMAXPROCS=1, about 0.2 ms of the ~4.5 ms a forward costs).
 * Added only when the caller has no GOMAXPROCS of its own, together with a
 * marker by which the command removes it again before anything reaches the
 * host or a local fallback. `out` has room for count(src) + 3 entries;
 * returns src itself when nothing is added. */
static char *const *helper_envp(char *const src[], char **out) {
    size_t n = 0;
    for (; src[n]; n++)
        if (strncmp(src[n], "GOMAXPROCS=", 11) == 0) return src;
    memcpy(out, src, n * sizeof(*out));
    out[n] = (char *)"GOMAXPROCS=1";
    out[n + 1] = (char *)FPS_ENV("GOMAXPROCS") "=1";
    out[n + 2] = NULL;
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

static int lookup_on_host(const struct fps_ctx *ctx, const char *name) {
    (void)ctx;
    char *argv[] = {(char *)FPS_NAME, (char *)FPS_MODE_LOOKUP, (char *)name, NULL};
    posix_spawn_file_actions_t fa;
    if (posix_spawn_file_actions_init(&fa) != 0) return FPS_LOOKUP_ERROR;
    posix_spawn_file_actions_addopen(&fa, 0, "/dev/null", O_RDONLY, 0);
    posix_spawn_file_actions_addopen(&fa, 1, "/dev/null", O_WRONLY, 0);
    pid_t pid;
    int rc;
    size_t ne = count(environ) + 3;
    if (ne <= STACK_ARGS) {
        char *ev[ne];
        rc = real_posix_spawn(&pid, FPS_HELPER, &fa, NULL, argv, (char **)helper_envp(environ, ev));
    } else {
        rc = real_posix_spawn(&pid, FPS_HELPER, &fa, NULL, argv, environ);
    }
    posix_spawn_file_actions_destroy(&fa);
    if (rc != 0) return FPS_LOOKUP_ERROR;
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

static void make_ctx(struct fps_ctx *ctx, struct ctx_bufs *b, char *const envp[]) {
    memset(ctx, 0, sizeof(*ctx));
    ctx->path = env_get(envp, "PATH");
    ctx->local_list = env_get(envp, FPS_ENV("LOCAL"));
    ctx->cwd = getcwd(b->cwd, sizeof(b->cwd));
    /* The cache is per working directory: what the host has depends on it
     * (mise, direnv). */
    const char *cache = env_get(envp, FPS_ENV("CACHE"));
    if (cache && cache[0] == '/' && ctx->cwd) {
        char key[17];
        fps_cwd_key(ctx->cwd, key);
        int n = snprintf(b->host, sizeof(b->host), "%s/" FPS_CACHE_CWD_DIR "/%s/" FPS_CACHE_HOST_DIR, cache, key);
        int m = snprintf(b->none, sizeof(b->none), "%s/" FPS_CACHE_CWD_DIR "/%s/" FPS_CACHE_NONE_DIR, cache, key);
        if (n > 0 && (size_t)n < sizeof(b->host) && m > 0 && (size_t)m < sizeof(b->none)) {
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
    if (debug())
        fprintf(stderr, FPS_NAME ": %s -> %s\n", file ? file : "(null)",
                r == FPS_HOST ? "host" : "sandbox");
    return r;
}

/* out (room for count(argv) + 5): [flatpark-spawn, --x-exec, target,
 * argv0, argv[1..]] */
static void forward_argv(char **out, const char *target, char *const argv[]) {
    size_t argc = count(argv);
    out[0] = (char *)FPS_NAME;
    out[1] = (char *)FPS_MODE_EXEC;
    out[2] = (char *)target;
    out[3] = argc > 0 ? argv[0] : (char *)target;
    for (size_t i = 1; i < argc; i++) out[i + 3] = argv[i];
    out[argc > 0 ? argc + 3 : 4] = NULL;
}

/* Runs fn(argv', envp') for the command with stack (or, when huge, heap)
 * arrays. */
typedef int (*launch_fn)(char *const argv[], char *const envp[], void *data);

static int with_helper_args(const char *target, char *const argv[], char *const envp[],
                            launch_fn fn, void *data) {
    char *const *src = envp ? envp : environ;
    size_t na = count(argv) + 5, ne = count(src) + 3;
    if (na <= STACK_ARGS && ne <= STACK_ARGS) {
        char *av[na], *ev[ne];
        forward_argv(av, target, argv);
        return fn(av, helper_envp(src, ev), data);
    }
    char **av = malloc(na * sizeof(*av)), **ev = malloc(ne * sizeof(*ev));
    int rc = -1;
    if (av && ev) {
        forward_argv(av, target, argv);
        rc = fn(av, helper_envp(src, ev), data);
    }
    free(av);
    free(ev);
    return rc;
}

static int do_execve(char *const argv[], char *const envp[], void *data) {
    (void)data;
    return real_execve(FPS_HELPER, argv, envp);
}

/* Returns only when the command could not be started; the caller then
 * goes on with the original exec. */
static void forward_exec(const char *target, char *const argv[], char *const envp[]) {
    int saved = errno;
    with_helper_args(target, argv, envp, do_execve, NULL);
    errno = saved;
}

struct spawn_args {
    pid_t *pid;
    const posix_spawn_file_actions_t *fa;
    const posix_spawnattr_t *attr;
};

static int do_spawn(char *const argv[], char *const envp[], void *data) {
    struct spawn_args *a = data;
    return real_posix_spawn(a->pid, FPS_HELPER, a->fa, a->attr, argv, envp);
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

static char **va_argv(const char *arg0, va_list ap) {
    size_t cap = 16, used = 0;
    char **argv = malloc(cap * sizeof(*argv));
    if (!argv) return NULL;
    for (const char *arg = arg0; arg; arg = va_arg(ap, const char *)) {
        if (used + 2 > cap) {
            cap *= 2;
            char **grown = realloc(argv, cap * sizeof(*argv));
            if (!grown) { free(argv); return NULL; }
            argv = grown;
        }
        argv[used++] = (char *)arg;
    }
    argv[used] = NULL;
    return argv;
}

EXPORT int execl(const char *file, const char *arg0, ...) {
    va_list ap;
    va_start(ap, arg0);
    char **argv = va_argv(arg0, ap);
    va_end(ap);
    if (!argv) { errno = ENOMEM; return -1; }
    int r = execve(file, argv, environ);
    int saved = errno;
    free(argv);
    errno = saved;
    return r;
}

EXPORT int execlp(const char *file, const char *arg0, ...) {
    va_list ap;
    va_start(ap, arg0);
    char **argv = va_argv(arg0, ap);
    va_end(ap);
    if (!argv) { errno = ENOMEM; return -1; }
    int r = execvp(file, argv);
    int saved = errno;
    free(argv);
    errno = saved;
    return r;
}

EXPORT int execle(const char *file, const char *arg0, ...) {
    va_list ap;
    va_start(ap, arg0);
    char **argv = va_argv(arg0, ap);
    char *const *envp = va_arg(ap, char *const *);
    va_end(ap);
    if (!argv) { errno = ENOMEM; return -1; }
    int r = execve(file, argv, envp);
    int saved = errno;
    free(argv);
    errno = saved;
    return r;
}

static int spawn_routed(spawn_fn real, int path_search, pid_t *pid, const char *file,
                        const posix_spawn_file_actions_t *fa, const posix_spawnattr_t *attr,
                        char *const argv[], char *const envp[]) {
    resolve();
    const char *target;
    if (decide(file, argv, envp, path_search, &target) == FPS_HOST) {
        struct spawn_args a = {pid, fa, attr};
        if (with_helper_args(target, argv, envp, do_spawn, &a) == 0) return 0;
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
    char d[24], t[24], p[24], c[8];
    snprintf(d, sizeof(d), "%d", domain);
    snprintf(t, sizeof(t), "%d", type);
    snprintf(p, sizeof(p), "%d", protocol);
    snprintf(c, sizeof(c), "%zu", count);
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
