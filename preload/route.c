/* See route.h. */
#define _GNU_SOURCE
#include "route.h"

#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

/* Paths that mean something only inside the sandbox. An exec whose arguments
 * name one of them stays local: the host would see a different file or none.
 * /etc is deliberately absent; see DESIGN.md "沙箱私有路径". */
static const char *const private_prefixes[] = {
    "/app", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32",
    "/tmp", "/var/tmp", "/run/flatpak", "/run/host", "/.flatpak-info",
    "/dev/fd", "/proc/self/fd", NULL,
};

void fps_cwd_key(const char *cwd, char out[17]) {
    unsigned long long h = 14695981039346656037ULL;
    for (const unsigned char *p = (const unsigned char *)cwd; *p; p++) {
        h ^= *p;
        h *= 1099511628211ULL;
    }
    snprintf(out, 17, "%016llx", h);
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

static int prefix_at(const struct fps_ctx *ctx, const char *s) {
    for (const char *const *p = private_prefixes; *p; p++) {
        if (ctx->tmp_shared && strcmp(*p, "/tmp") == 0) continue;
        size_t n = strlen(*p);
        if (strncmp(s, *p, n) == 0 && boundary_after(s[n])) return 1;
    }
    return 0;
}

int fps_is_private_path(const struct fps_ctx *ctx, const char *path) {
    return path && path[0] == '/' && prefix_at(ctx, path);
}

/* True when a sandbox-private path appears in arg as a token of its own:
 * the whole argument, `--opt=/app/x`, `PATH=/usr/bin:...`, or a word inside
 * a shell command string (`sh -c 'cat /app/x'`). */
int fps_mentions_private_path(const struct fps_ctx *ctx, const char *arg) {
    if (!arg) return 0;
    for (size_t i = 0; arg[i]; i++) {
        if (arg[i] != '/') continue;
        if (i > 0 && !boundary_before(arg[i - 1])) continue;
        if (prefix_at(ctx, arg + i)) return 1;
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

int fps_path_has(const struct fps_ctx *ctx, const char *name) {
    if (!ctx->path || !fps_valid_name(name)) return 0;
    char buf[4096];
    const char *p = ctx->path;
    while (*p) {
        const char *end = strchr(p, ':');
        size_t len = end ? (size_t)(end - p) : strlen(p);
        if (len > 0 && p[0] == '/') {
            int n = snprintf(buf, sizeof(buf), "%.*s/%s", (int)len, p, name);
            struct stat st;
            if (n > 0 && (size_t)n < sizeof(buf) && stat(buf, &st) == 0 &&
                S_ISREG(st.st_mode) && access(buf, X_OK) == 0)
                return 1;
        }
        if (!end) break;
        p = end + 1;
    }
    return 0;
}

int fps_cache_state(const struct fps_ctx *ctx, const char *name, int local) {
    if (!fps_valid_name(name)) return FPS_CACHE_UNKNOWN;
    char buf[4096];
    struct stat st;
    int n;
    if (ctx->cache_host) {
        n = snprintf(buf, sizeof(buf), "%s/%s", ctx->cache_host, name);
        if (n > 0 && (size_t)n < sizeof(buf) && stat(buf, &st) == 0) return FPS_CACHE_HOST;
    }
    if (ctx->cache_none) {
        n = snprintf(buf, sizeof(buf), "%s/%s", ctx->cache_none, name);
        if (n > 0 && (size_t)n < sizeof(buf) && stat(buf, &st) == 0) {
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
    if (!path_search && fps_is_path_candidate(ctx, file, argv))
        return route_name(ctx, fps_base_name(file), argv, target);
    /* An explicit absolute path: the sandbox's own file wins, sandbox-only
     * locations never leave, anything else missing here is tried on the
     * host (`/usr/bin/docker`, `~/.local/bin/mise` without a grant). */
    struct stat st;
    if (stat(file, &st) == 0) return FPS_LOCAL;
    if (errno != ENOENT && errno != ENOTDIR) return FPS_LOCAL;
    if (fps_is_private_path(ctx, file)) return FPS_LOCAL;
    *target = file;
    return FPS_HOST;
}
