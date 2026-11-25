// Package routetest exposes preload/route.c, the C decision code of
// libflatpark-spawn.so, to Go tests through cgo. Only route.c is compiled
// here, never the libc hooks of preload.c. Nothing imports this package.
package routetest
