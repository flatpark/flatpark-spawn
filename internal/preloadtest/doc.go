// Package preloadtest builds libflatpark-spawn.so from preload/ with the
// extension mount pointed at a temporary directory, and runs it outside a
// sandbox against a fake flatpark-spawn command standing in for the host.
// It covers what internal/routetest cannot: the libc hooks themselves, in
// particular the sticky preload (LD_PRELOAD removed by an intermediate
// process) and the allocator-free exec path. Nothing imports this package.
package preloadtest
