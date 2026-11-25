/* See route.h. */
#define _GNU_SOURCE
#include "route.h"

#include <errno.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Two lists, for two questions; see DESIGN.md "沙箱私有路径".
 *
 * private_prefixes: an exec whose arguments or cwd name one of these stays
 * local (if the sandbox has the command). The command would read the
 * sandbox's file; the host has a different one there, or none. /usr, /bin
 * and /lib* are on it: the runtime's tree is not the host's. /etc is
 * deliberately absent. */
static const char *const private_prefixes[] = {
    "/app", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32",
    "/tmp", "/var/tmp", "/run/flatpak", "/run/host", "/.flatpak-info",
    "/dev/fd", "/proc/self/fd", NULL,
};

/* sandbox_only_prefixes: an explicit absolute exec target that is missing in
 * the sandbox stays local (native ENOENT) only under these, the locations
 * the host does not have or has with another meaning. /usr, /bin, /sbin and
 * /lib* are not on it: the host has those trees too, and a file the
 * runtime's tree lacks can only be the host's (`/usr/bin/nvim`). Every entry
 * is also in private_prefixes. internal/sandbox's envPrivate is the same
 * list minus the fd paths (TestSandboxOnlyMatchesHostEnv). */
static const char *const sandbox_only_prefixes[] = {
    "/app", "/tmp", "/var/tmp", "/run/flatpak", "/run/host", "/.flatpak-info",
    "/dev/fd", "/proc/self/fd", NULL,
};

/* Everything below runs on the exec path, also in the child of a fork in a
 * multithreaded process and in vfork children: no malloc, no stdio, no
 * locks; only the async-signal-safe string functions of POSIX.1-2008 TC2
 * and plain system calls (stat, access). See DESIGN.md "async-signal-safety". */

int fps_append(char *out, size_t cap, size_t *pos, const char *s, size_t n) {
    if (*pos >= cap || n >= cap - *pos) return -1;
    memcpy(out + *pos, s, n);
    *pos += n;
    out[*pos] = '\0';
    return 0;
}

void fps_cwd_key(const char *cwd, char out[17]) {
    unsigned long long h = 14695981039346656037ULL;
    for (const unsigned char *p = (const unsigned char *)cwd; *p; p++) {
        h ^= *p;
        h *= 1099511628211ULL;
    }
    static const char hex[] = "0123456789abcdef";
    for (int i = 15; i >= 0; i--, h >>= 4) out[i] = hex[h & 15];
    out[16] = '\0';
}

const char *fps_base_name(const char *path) {
    const char *slash = strrchr(path, '/');
    return slash ? slash + 1 : path;
}

static int boundary_before(char c) {
    return strchr(" \t\n'\"=:;(`,<>|&@{", c) != NULL;
}

static int boundary_after(char c) {
    return c == '\0' || c == '/' || strchr(" \t\n'\":;)`,<>|&}", c) != NULL;
}

static int prefix_at(const struct fps_ctx *ctx, const char *const *list, const char *s) {
    for (const char *const *p = list; *p; p++) {
        if (ctx->tmp_shared && strcmp(*p, "/tmp") == 0) continue;
        size_t n = strlen(*p);
        if (strncmp(s, *p, n) == 0 && boundary_after(s[n])) return 1;
    }
    return 0;
}

int fps_is_private_path(const struct fps_ctx *ctx, const char *path) {
    return path && path[0] == '/' && prefix_at(ctx, private_prefixes, path);
}

int fps_is_sandbox_only_path(const struct fps_ctx *ctx, const char *path) {
    return path && path[0] == '/' && prefix_at(ctx, sandbox_only_prefixes, path);
}

/* True when a sandbox-private path appears in arg as a token of its own:
 * the whole argument, `--opt=/app/x`, `PATH=/usr/bin:...`, or a word inside
 * a shell command string (`sh -c 'cat /app/x'`). */
int fps_mentions_private_path(const struct fps_ctx *ctx, const char *arg) {
    if (!arg) return 0;
    for (size_t i = 0; arg[i]; i++) {
        if (arg[i] != '/') continue;
        if (i > 0 && !boundary_before(arg[i - 1])) continue;
        if (prefix_at(ctx, private_prefixes, arg + i)) return 1;
    }
    return 0;
}

int fps_list_has(const char *list, const char *name) {
    if (!list || !name || !*name) return 0;
    size_t n = strlen(name);
    const char *p = list;
    while (*p) {
        const char *end = strchr(p, ':');
        size_t len = end ? (size_t)(end - p) : strlen(p);
        if (len == n && strncmp(p, name, n) == 0) return 1;
        if (!end) break;
        p = end + 1;
    }
    return 0;
}

int fps_valid_name(const char *name) {
    if (!name || !*name || strchr(name, '/')) return 0;
    if (strcmp(name, ".") == 0 || strcmp(name, "..") == 0) return 0;
    return strlen(name) < 200;
}

/* glibc's execvp calls the internal __execve, which is not hooked; but
 * Python's subprocess and bash search PATH themselves and call execve on
 * each candidate, with argv[0] still the bare name. Such a call is treated
 * as an exec of that name. An explicit absolute path keeps its argv[0]
 * (`/usr/bin/cat`) and is not a candidate. */
int fps_is_path_candidate(const struct fps_ctx *ctx, const char *file, char *const argv[]) {
    if (!file || file[0] != '/' || !argv || !argv[0] || strchr(argv[0], '/')) return 0;
    const char *base = fps_base_name(file);
    if (strcmp(base, argv[0]) != 0) return 0;
    size_t dir_len = (size_t)(base - file) - 1;
    if (dir_len == 0 || !ctx->path) return 0;
    const char *p = ctx->path;
    while (*p) {
        const char *end = strchr(p, ':');
        size_t len = end ? (size_t)(end - p) : strlen(p);
        if (len == dir_len && strncmp(p, file, len) == 0) return 1;
        if (!end) break;
        p = end + 1;
    }
    return 0;
}

/* Whether the n bytes at dir name the same directory as shim, trailing
 * slashes aside (no symlinks are resolved: the shim directory is the
 * literal path enable.sh put in PATH). */
static int is_shim_dir(const struct fps_ctx *ctx, const char *dir, size_t n) {
    const char *shim = ctx->shim_dir;
    if (!shim || shim[0] != '/') return 0;
    size_t m = strlen(shim);
    while (m > 1 && shim[m - 1] == '/') m--;
    while (n > 1 && dir[n - 1] == '/') n--;
    return n == m && strncmp(dir, shim, n) == 0;
}

int fps_in_shim_dir(const struct fps_ctx *ctx, const char *file) {
    if (!file || file[0] != '/') return 0;
    const char *base = fps_base_name(file);
    return fps_valid_name(base) && is_shim_dir(ctx, file, (size_t)(base - file) - 1);
}

int fps_path_has(const struct fps_ctx *ctx, const char *name) {
    if (!ctx->path || !fps_valid_name(name)) return 0;
    char buf[4096];
    const char *p = ctx->path;
    while (*p) {
        const char *end = strchr(p, ':');
        size_t len = end ? (size_t)(end - p) : strlen(p);
        if (len > 0 && p[0] == '/' && !is_shim_dir(ctx, p, len)) {
            size_t pos = 0;
            struct stat st;
            if (fps_append(buf, sizeof(buf), &pos, p, len) == 0 &&
                fps_append(buf, sizeof(buf), &pos, "/", 1) == 0 &&
                fps_append(buf, sizeof(buf), &pos, name, strlen(name)) == 0 && stat(buf, &st) == 0 &&
                S_ISREG(st.st_mode) && access(buf, X_OK) == 0)
                return 1;
        }
        if (!end) break;
        p = end + 1;
    }
    return 0;
}

static int cache_path(char *buf, size_t cap, const char *dir, const char *name) {
    size_t pos = 0;
    if (fps_append(buf, cap, &pos, dir, strlen(dir)) < 0 ||
        fps_append(buf, cap, &pos, "/", 1) < 0 ||
        fps_append(buf, cap, &pos, name, strlen(name)) < 0)
        return -1;
    return 0;
}

int fps_cache_state(const struct fps_ctx *ctx, const char *name, int local) {
    if (!fps_valid_name(name)) return FPS_CACHE_UNKNOWN;
    char buf[4096];
    struct stat st;
    if (ctx->cache_host && cache_path(buf, sizeof(buf), ctx->cache_host, name) == 0 &&
        stat(buf, &st) == 0)
        return FPS_CACHE_HOST;
    if (ctx->cache_none && cache_path(buf, sizeof(buf), ctx->cache_none, name) == 0) {
        if (stat(buf, &st) == 0) {
            long long age = ctx->now - (long long)st.st_mtim.tv_sec;
            long long ttl = local ? FPS_NEG_TTL_LOCAL : FPS_NEG_TTL_MISSING;
            /* A future mtime (clock moved back) is not trusted. */
            if (age >= 0 && age < ttl) return FPS_CACHE_NONE;
        }
    }
    return FPS_CACHE_UNKNOWN;
}

static int args_private(const struct fps_ctx *ctx, char *const argv[]) {
    if (fps_is_private_path(ctx, ctx->cwd)) return 1;
    for (size_t i = 1; argv && argv[i]; i++)
        if (fps_mentions_private_path(ctx, argv[i])) return 1;
    return 0;
}

static enum fps_route route_name(const struct fps_ctx *ctx, const char *name,
                                 char *const argv[], const char **target) {
    if (!fps_valid_name(name) || fps_list_has(ctx->local_list, name)) return FPS_LOCAL;
    fps_local_exists_fn exists = ctx->local_exists ? ctx->local_exists : fps_path_has;
    int local = exists(ctx, name);
    /* Arguments only the sandbox can resolve: run it here, if it is here. */
    if (local && args_private(ctx, argv)) return FPS_LOCAL;
    switch (fps_cache_state(ctx, name, local)) {
    case FPS_CACHE_HOST:
        *target = name;
        return FPS_HOST;
    case FPS_CACHE_NONE:
        return FPS_LOCAL;
    default:
        break;
    }
    if (ctx->host_lookup && ctx->host_lookup(ctx, name) == FPS_LOOKUP_HOST) {
        *target = name;
        return FPS_HOST;
    }
    return FPS_LOCAL;
}

enum fps_route fps_route_exec(const struct fps_ctx *ctx, const char *file,
                              char *const argv[], int path_search,
                              const char **target) {
    *target = NULL;
    if (!file || !*file) return FPS_LOCAL;
    if (!strchr(file, '/')) return route_name(ctx, file, argv, target);
    /* Relative paths depend on a directory the host may not share. */
    if (file[0] != '/') return FPS_LOCAL;
    /* A shim (enable.sh's directory of links to the command, first in
     * PATH): route the name, as if the caller had asked for it bare, by
     * whatever way it reached the file (execvp's own search, Python's and
     * bash's candidates, or a path a program found on disk). Where the
     * name stays local the shim runs, and its shim mode finds the
     * sandbox's copy. */
    if (fps_in_shim_dir(ctx, file)) return route_name(ctx, fps_base_name(file), argv, target);
    if (!path_search && fps_is_path_candidate(ctx, file, argv))
        return route_name(ctx, fps_base_name(file), argv, target);
    /* An explicit absolute path: the sandbox's own file wins (its loader,
     * `/usr/bin/python3`), sandbox-only locations never leave (`/app/...`),
     * anything else missing here is tried on the host unchanged
     * (`/usr/bin/docker`, `~/.local/bin/mise` without a grant). The
     * arguments are not consulted: here it could only fail with ENOENT. */
    struct stat st;
    if (stat(file, &st) == 0) return FPS_LOCAL;
    if (errno != ENOENT && errno != ENOTDIR) return FPS_LOCAL;
    if (fps_is_sandbox_only_path(ctx, file)) return FPS_LOCAL;
    *target = file;
    return FPS_HOST;
}
