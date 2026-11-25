//go:build cgo

package routetest

/*
#cgo CFLAGS: -std=c11 -Wall -Wextra -Werror
#include <stdlib.h>
#include "../../preload/route.h"

void rt_reset(const char *host);
int rt_lookups(void);
int rt_route(const char *path, const char *local, const char *cache_host, const char *cache_none,
             const char *cwd, int tmp_shared, long long now, const char *file, char **argv,
             int path_search, char *target, int target_len);
int rt_mentions_private(const char *arg, int tmp_shared);
int rt_cache_state(const char *cache_host, const char *cache_none, long long now,
                   const char *name, int local);
void rt_cwd_key(const char *cwd, char *out);
*/
import "C"

import "unsafe"

// Route constants and TTLs from route.h.
const (
	Local         = int(C.FPS_LOCAL)
	Host          = int(C.FPS_HOST)
	CacheUnknown  = int(C.FPS_CACHE_UNKNOWN)
	CacheHost     = int(C.FPS_CACHE_HOST)
	CacheNone     = int(C.FPS_CACHE_NONE)
	NegTTLLocal   = int64(C.FPS_NEG_TTL_LOCAL)
	NegTTLMissing = int64(C.FPS_NEG_TTL_MISSING)
)

// Ctx is the input of one decision.
type Ctx struct {
	Path      string // sandbox PATH
	Local     string // FLATPARK_SPAWN_LOCAL
	CacheHost string // "" for no cache
	CacheNone string
	Cwd       string
	TmpShared bool
	Now       int64
	HostHas   string // ':'-separated names the fake host lookup reports
}

func cstr(s string) *C.char {
	if s == "" {
		return nil
	}
	return C.CString(s)
}

func free(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

func b2i(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Route runs fps_route_exec. It returns the route, the host target and how
// many times the host was asked.
func Route(c Ctx, file string, argv []string, pathSearch bool) (int, string, int) {
	host := cstr(c.HostHas)
	C.rt_reset(host)
	free(host)
	cargv := C.malloc(C.size_t(len(argv)+1) * C.size_t(unsafe.Sizeof(uintptr(0))))
	defer C.free(cargv)
	slots := unsafe.Slice((**C.char)(cargv), len(argv)+1)
	for i, a := range argv {
		slots[i] = C.CString(a)
	}
	slots[len(argv)] = nil
	defer func() {
		for i := range argv {
			C.free(unsafe.Pointer(slots[i]))
		}
	}()
	strs := []*C.char{cstr(c.Path), cstr(c.Local), cstr(c.CacheHost), cstr(c.CacheNone), cstr(c.Cwd), cstr(file)}
	defer func() {
		for _, s := range strs {
			free(s)
		}
	}()
	var target [4096]C.char
	r := C.rt_route(strs[0], strs[1], strs[2], strs[3], strs[4], b2i(c.TmpShared), C.longlong(c.Now),
		strs[5], (**C.char)(cargv), b2i(pathSearch), &target[0], C.int(len(target)))
	return int(r), C.GoString(&target[0]), int(C.rt_lookups())
}

// MentionsPrivate runs fps_mentions_private_path.
func MentionsPrivate(arg string, tmpShared bool) bool {
	a := C.CString(arg)
	defer free(a)
	return C.rt_mentions_private(a, b2i(tmpShared)) != 0
}

// CacheState runs fps_cache_state.
func CacheState(cacheHost, cacheNone string, now int64, name string, local bool) int {
	h, n, nm := cstr(cacheHost), cstr(cacheNone), cstr(name)
	defer free(h)
	defer free(n)
	defer free(nm)
	return int(C.rt_cache_state(h, n, C.longlong(now), nm, b2i(local)))
}

// CwdKey runs fps_cwd_key.
func CwdKey(cwd string) string {
	c := C.CString(cwd)
	defer C.free(unsafe.Pointer(c))
	var out [17]C.char
	C.rt_cwd_key(c, &out[0])
	return C.GoString(&out[0])
}
