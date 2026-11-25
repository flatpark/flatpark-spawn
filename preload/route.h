/* Where an exec goes: the sandbox or the host. Pure decision code, no libc
 * hooks, so that the Go tests can drive it through cgo (internal/routetest).
 * The rules and their reasons are in DESIGN.md, "判定表". */
#ifndef FPS_ROUTE_H
#define FPS_ROUTE_H

#include <stddef.h>

enum fps_route { FPS_LOCAL = 0, FPS_HOST = 1 };

/* How long "the host has no such command" is trusted, in seconds. A name
 * the sandbox has (an app's own /app/bin tool) keeps running locally for
 * longer; a name neither side has is asked again soon, so that a command
 * installed on the host right after a failed call shows up quickly. */
#define FPS_NEG_TTL_LOCAL 60
#define FPS_NEG_TTL_MISSING 3

/* Results of fps_host_lookup_fn. */
#define FPS_LOOKUP_HOST 1
#define FPS_LOOKUP_NONE 0
#define FPS_LOOKUP_ERROR (-1)

struct fps_ctx;
typedef int (*fps_local_exists_fn)(const struct fps_ctx *, const char *name);
typedef int (*fps_host_lookup_fn)(const struct fps_ctx *, const char *name);

struct fps_ctx {
    const char *path;       /* the sandbox's PATH */
    const char *local_list; /* FLATPARK_SPAWN_LOCAL: names kept local, ':'-separated */
    const char *cache_host; /* <cache>/host, or NULL when there is no cache */
    const char *cache_none; /* <cache>/none, or NULL */
    const char *cwd;        /* current directory, or NULL */
    const char *shim_dir;   /* FLATPARK_SPAWN_SHIMS (absolute), or NULL */
    int tmp_shared;         /* 1 when the app was granted the host's /tmp */
    long long now;          /* seconds, same clock as file mtimes */
    fps_local_exists_fn local_exists;
    fps_host_lookup_fn host_lookup;
    void *user;
};

/* Decides an exec of `file` (as passed to execve/execvp/posix_spawn*).
 * path_search: the caller searches PATH itself (execvp, posix_spawnp).
 * On FPS_HOST, *target is what to run on the host: a bare name or an
 * absolute path. */
enum fps_route fps_route_exec(const struct fps_ctx *ctx, const char *file,
                              char *const argv[], int path_search,
                              const char **target);

/* Building blocks, exported for the tests. */
int fps_is_private_path(const struct fps_ctx *ctx, const char *path);
int fps_is_sandbox_only_path(const struct fps_ctx *ctx, const char *path);
int fps_mentions_private_path(const struct fps_ctx *ctx, const char *arg);
int fps_list_has(const char *list, const char *name);
int fps_valid_name(const char *name);
int fps_is_path_candidate(const struct fps_ctx *ctx, const char *file, char *const argv[]);
/* Whether file is <shim_dir>/<valid name> (DESIGN.md, shim). */
int fps_in_shim_dir(const struct fps_ctx *ctx, const char *file);

/* Cache state of a bare name. */
#define FPS_CACHE_UNKNOWN 0
#define FPS_CACHE_HOST 1
#define FPS_CACHE_NONE 2
int fps_cache_state(const struct fps_ctx *ctx, const char *name, int local);

/* Default local_exists: an executable regular file in ctx->path, the shim
 * directory left out (a shim is not the sandbox's own copy). */
int fps_path_has(const struct fps_ctx *ctx, const char *name);

const char *fps_base_name(const char *path);

/* Appends n bytes of s to out (capacity cap, NUL-terminated, *pos bytes
 * used); -1 when it does not fit. snprintf without stdio, for the exec
 * path (async-signal-safe). */
int fps_append(char *out, size_t cap, size_t *pos, const char *s, size_t n);

/* The cache directory name of a working directory: FNV-64a of the path as
 * 16 hex digits (internal/agent.Key computes the same). out: 17 bytes. */
void fps_cwd_key(const char *cwd, char out[17]);

#endif
