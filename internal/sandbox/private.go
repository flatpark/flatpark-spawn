package sandbox

import "strings"

// The argument rule of preload/route.c (private_prefixes,
// fps_mentions_private_path, args_private), for the shim mode: a shim runs
// in processes without the preload library and must decide the same way.
// internal/routetest compares the two (TestPrivateRuleMatchesC); see
// DESIGN.md, 沙箱私有路径.
var privatePrefixes = []string{
	"/app", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32",
	"/tmp", "/var/tmp", "/run/flatpak", "/run/host", "/.flatpak-info",
	"/dev/fd", "/proc/self/fd",
}

const (
	privateBefore = " \t\n'\"=:;(`,<>|&@{"
	privateAfter  = " \t\n'\":;)`,<>|&}" // and the end, and '/'
)

func privateAt(s string, tmpShared bool) bool {
	for _, p := range privatePrefixes {
		if tmpShared && p == "/tmp" {
			continue
		}
		if !strings.HasPrefix(s, p) {
			continue
		}
		if len(s) == len(p) || s[len(p)] == '/' || strings.IndexByte(privateAfter, s[len(p)]) >= 0 {
			return true
		}
	}
	return false
}

// IsPrivatePath: path itself starts with a sandbox-private prefix
// (fps_is_private_path; used for the working directory).
func IsPrivatePath(path string, tmpShared bool) bool {
	return strings.HasPrefix(path, "/") && privateAt(path, tmpShared)
}

// MentionsPrivate: a private path appears in arg as a token of its own
// (fps_mentions_private_path).
func MentionsPrivate(arg string, tmpShared bool) bool {
	for i := 0; i < len(arg); i++ {
		if arg[i] != '/' {
			continue
		}
		if i > 0 && strings.IndexByte(privateBefore, arg[i-1]) < 0 {
			continue
		}
		if privateAt(arg[i:], tmpShared) {
			return true
		}
	}
	return false
}

// ArgsPrivate: the working directory or an argument (argv[1:]) is
// sandbox-private (args_private).
func ArgsPrivate(cwd string, args []string, tmpShared bool) bool {
	if IsPrivatePath(cwd, tmpShared) {
		return true
	}
	for _, a := range args {
		if MentionsPrivate(a, tmpShared) {
			return true
		}
	}
	return false
}
