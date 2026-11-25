//go:build cgo

/* Test harness around preload/route.c (see route.go). */
#include "../../preload/route.c"

#include <stdlib.h>

static char host_names[8192];
static int lookups;

static int test_lookup(const struct fps_ctx *ctx, const char *name) {
    (void)ctx;
    lookups++;
    return fps_list_has(host_names, name) ? FPS_LOOKUP_HOST : FPS_LOOKUP_NONE;
}

void rt_reset(const char *host) {
    snprintf(host_names, sizeof(host_names), "%s", host ? host : "");
    lookups = 0;
}

int rt_lookups(void) { return lookups; }

static struct fps_ctx make(const char *path, const char *local, const char *cache_host,
                           const char *cache_none, const char *cwd, int tmp_shared, long long now) {
    struct fps_ctx c;
    memset(&c, 0, sizeof(c));
    c.path = path;
    c.local_list = local;
    c.cache_host = cache_host;
    c.cache_none = cache_none;
    c.cwd = cwd;
    c.tmp_shared = tmp_shared;
    c.now = now;
    c.local_exists = fps_path_has;
    c.host_lookup = test_lookup;
    return c;
}

int rt_route(const char *path, const char *local, const char *cache_host, const char *cache_none,
             const char *cwd, int tmp_shared, long long now, const char *file, char **argv,
             int path_search, char *target, int target_len) {
    struct fps_ctx c = make(path, local, cache_host, cache_none, cwd, tmp_shared, now);
    const char *t = NULL;
    int r = fps_route_exec(&c, file, argv, path_search, &t);
    snprintf(target, (size_t)target_len, "%s", t ? t : "");
    return r;
}

int rt_mentions_private(const char *arg, int tmp_shared) {
    struct fps_ctx c = make(NULL, NULL, NULL, NULL, NULL, tmp_shared, 0);
    return fps_mentions_private_path(&c, arg);
}

int rt_cache_state(const char *cache_host, const char *cache_none, long long now,
                   const char *name, int local) {
    struct fps_ctx c = make(NULL, NULL, cache_host, cache_none, NULL, 0, now);
    return fps_cache_state(&c, name, local);
}

void rt_cwd_key(const char *cwd, char *out) { fps_cwd_key(cwd, out); }
