//go:build cgo

package routetest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flatpark/flatpark-spawn/internal/agent"
	"github.com/flatpark/flatpark-spawn/internal/sandbox"
)

// sandboxFS fakes the sandbox's PATH: /app/bin and /usr/bin stand-ins with
// the given executables.
type sandboxFS struct {
	appBin, usrBin, cache string
}

func newFS(t *testing.T, app, usr []string) sandboxFS {
	t.Helper()
	root := t.TempDir()
	fs := sandboxFS{
		appBin: filepath.Join(root, "app", "bin"),
		usrBin: filepath.Join(root, "usr", "bin"),
		cache:  filepath.Join(root, "cache"),
	}
	for dir, list := range map[string][]string{fs.appBin: app, fs.usrBin: usr} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, n := range list {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fs
}

const testCwd = "/home/user/project"

// cache is the Go side's view of the cache for the test cwd; ctx hands the
// preload library the same directories, as preload.c does.
func (fs sandboxFS) cacheFor() sandbox.Cache {
	return sandbox.Cache{Dir: fs.cache, Cwd: testCwd, Now: time.Now}
}

func (fs sandboxFS) ctx(hostHas string) Ctx {
	return Ctx{
		Path:      fs.appBin + ":" + fs.usrBin,
		CacheHost: fs.cacheFor().HostDir(),
		CacheNone: fs.cacheFor().NoneDir(),
		Cwd:       testCwd,
		Now:       time.Now().Unix(),
		HostHas:   hostHas,
	}
}

func TestSameNameGoesToHost(t *testing.T) {
	fs := newFS(t, []string{"apptool"}, []string{"cat", "sh"})
	c := fs.ctx("cat:sh:git")
	for _, tc := range []struct {
		name, file string
		argv       []string
		want       int
		target     string
	}{
		{"both have cat", "cat", []string{"cat", "/etc/os-release"}, Host, "cat"},
		{"host only", "git", []string{"git", "status"}, Host, "git"},
		{"sandbox only", "apptool", []string{"apptool"}, Local, ""},
		{"nowhere", "nosuch", []string{"nosuch"}, Local, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, target, _ := Route(c, tc.file, tc.argv, true)
			if r != tc.want || target != tc.target {
				t.Fatalf("got %d %q, want %d %q", r, target, tc.want, tc.target)
			}
		})
	}
}

func TestPrivatePathsStayLocal(t *testing.T) {
	fs := newFS(t, nil, []string{"cat", "sh", "python3"})
	c := fs.ctx("cat:sh:python3:node")
	for _, tc := range []struct {
		name string
		argv []string
		want int
	}{
		{"app file", []string{"cat", "/app/share/marker"}, Local},
		{"app dir itself", []string{"cat", "/app"}, Local},
		{"runtime usr", []string{"cat", "/usr/share/doc/x"}, Local},
		{"runtime bin", []string{"sh", "/bin/script"}, Local},
		{"option value", []string{"python3", "--prefix=/app"}, Local},
		{"inside a shell string", []string{"sh", "-c", "cat /app/share/marker | wc -l"}, Local},
		{"quoted in a shell string", []string{"sh", "-c", "cat '/app/x'"}, Local},
		{"PATH-like value", []string{"sh", "-c", "PATH=/usr/bin:$PATH foo"}, Local},
		{"private tmp", []string{"cat", "/tmp/x"}, Local},
		{"private var tmp", []string{"cat", "/var/tmp/x"}, Local},
		{"process substitution", []string{"cat", "/dev/fd/63"}, Local},
		{"proc fd", []string{"cat", "/proc/self/fd/3"}, Local},
		{"flatpak-info", []string{"cat", "/.flatpak-info"}, Local},
		{"host etc", []string{"cat", "/etc/os-release"}, Host},
		{"home", []string{"cat", "/home/user/x"}, Host},
		{"not a prefix: /application", []string{"cat", "/application/x"}, Host},
		{"not a prefix: /usrlocal", []string{"cat", "/usrlocal"}, Host},
		{"inside a URL", []string{"cat", "https://example.com/app/x"}, Host},
		{"relative", []string{"cat", "notes.txt"}, Host},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := Route(c, tc.argv[0], tc.argv, true)
			if r != tc.want {
				t.Fatalf("%v: got %d, want %d", tc.argv, r, tc.want)
			}
		})
	}

	t.Run("private cwd", func(t *testing.T) {
		c := c
		for _, cwd := range []string{"/app/share", "/usr/share", "/tmp"} {
			c.Cwd = cwd
			if r, _, _ := Route(c, "cat", []string{"cat", "notes.txt"}, true); r != Local {
				t.Fatalf("cwd %s must stay local", cwd)
			}
		}
	})
	t.Run("shared tmp", func(t *testing.T) {
		c := c
		c.TmpShared = true
		if r, _, _ := Route(c, "cat", []string{"cat", "/tmp/x"}, true); r != Host {
			t.Fatal("with --filesystem=/tmp, /tmp is the host's")
		}
	})
	t.Run("private path but nothing local", func(t *testing.T) {
		// node only exists on the host: running it there is the only option.
		if r, target, _ := Route(c, "node", []string{"node", "/app/x.js"}, true); r != Host || target != "node" {
			t.Fatalf("got %d %q", r, target)
		}
	})
}

func TestMentionsPrivate(t *testing.T) {
	for arg, want := range map[string]bool{
		"/app":            true,
		"/app/":           true,
		"x=/usr/lib":      true,
		"(cd /tmp && ls)": true,
		"/apps":           false,
		"a/app/b":         false,
		"-I/usr/include":  false, // documented gap: no boundary before /usr
		"/etc/hosts":      false,
		"":                false,
	} {
		if got := MentionsPrivate(arg, false); got != want {
			t.Errorf("%q: got %v, want %v", arg, got, want)
		}
	}
	if MentionsPrivate("/tmp/x", true) {
		t.Error("/tmp is not private when shared")
	}
}

func TestLocalList(t *testing.T) {
	fs := newFS(t, nil, []string{"cat", "python3"})
	c := fs.ctx("cat:python3:ssh")
	c.Local = "python3:ssh"
	for _, name := range []string{"python3", "ssh"} {
		r, _, lookups := Route(c, name, []string{name}, true)
		if r != Local || lookups != 0 {
			t.Fatalf("%s: got %d after %d lookups, want local without asking", name, r, lookups)
		}
	}
	if r, _, _ := Route(c, "cat", []string{"cat"}, true); r != Host {
		t.Fatal("unlisted names still go to the host")
	}
}

func TestPathCandidates(t *testing.T) {
	fs := newFS(t, nil, []string{"cat", "fps-own"})
	c := fs.ctx("cat")
	appCat := filepath.Join(fs.appBin, "cat")
	usrCat := filepath.Join(fs.usrBin, "cat")

	// Python/bash: execve(dir/name) with argv[0] == name, for each PATH dir.
	if r, target, _ := Route(c, appCat, []string{"cat", "/etc/os-release"}, false); r != Host || target != "cat" {
		t.Fatalf("PATH candidate: got %d %q", r, target)
	}
	// The same file with an explicit absolute argv[0] is the app's choice.
	if r, _, _ := Route(c, usrCat, []string{usrCat, "/etc/os-release"}, false); r != Local {
		t.Fatal("explicit absolute path that exists must stay local")
	}
	// A candidate the host lacks runs the sandbox's copy (on the next candidate).
	ownApp := filepath.Join(fs.appBin, "fps-own")
	if r, _, _ := Route(c, ownApp, []string{"fps-own"}, false); r != Local {
		t.Fatal("candidate of a sandbox-only name must stay local")
	}
	// Not in PATH: not a candidate.
	other := "/home/fps-nonexistent/bin/cat" // not under a private prefix
	if r, target, _ := Route(c, other, []string{"cat"}, false); r != Host || target != other {
		t.Fatalf("non-PATH absolute missing path: got %d %q", r, target)
	}
}

func TestAbsoluteAndRelative(t *testing.T) {
	fs := newFS(t, nil, nil)
	c := fs.ctx("")
	missing := "/home/user/.local/bin/mise"
	if r, target, lookups := Route(c, missing, []string{missing, "--version"}, false); r != Host || target != missing || lookups != 0 {
		t.Fatalf("missing absolute path: got %d %q %d", r, target, lookups)
	}
	if r, _, _ := Route(c, "/app/extra/missing", []string{"/app/extra/missing"}, false); r != Local {
		t.Fatal("a missing /app path must not go to the host")
	}
	if r, _, _ := Route(c, "./tool", []string{"./tool"}, false); r != Local {
		t.Fatal("relative paths stay local")
	}
	if r, _, _ := Route(c, "", []string{""}, false); r != Local {
		t.Fatal("empty file stays local")
	}
}

// missingName is a file name no test machine has, so that the paths built
// from it do not exist here (the decision stats the real file system).
const missingName = "fps-routetest-missing-8b1f"

// TestAbsoluteTargetMissingInSandbox: an explicit absolute target that is
// not in the sandbox. Under /usr, /bin, /sbin and /lib* it can only mean the
// host's file (the host has those trees too); under the locations the host
// does not have, or has with another meaning, it stays here (native ENOENT).
// The target goes to the host unchanged, without a lookup: when the host
// lacks it too, the command reports ENOENT and exits 127 (see e2e).
func TestAbsoluteTargetMissingInSandbox(t *testing.T) {
	fs := newFS(t, nil, nil)
	c := fs.ctx("")
	for _, file := range []string{
		"/usr/bin/" + missingName,
		"/bin/" + missingName,
		"/sbin/" + missingName,
		"/usr/sbin/" + missingName,
		"/usr/local/bin/" + missingName,
		"/usr/lib/" + missingName + "/helper",
		"/usr/libexec/" + missingName,
		"/lib/x86_64-linux-gnu/" + missingName,
		"/lib64/" + missingName,
		"/lib32/" + missingName,
		"/usr/bin/nope-" + missingName, // missing on the host as well
	} {
		r, target, lookups := Route(c, file, []string{file, "--version"}, false)
		if r != Host || target != file || lookups != 0 {
			t.Errorf("%s: got %d %q after %d lookups, want host %q without asking", file, r, target, lookups, file)
		}
	}
	for _, file := range []string{
		"/app/bin/" + missingName,
		"/app",
		"/run/host/usr/bin/" + missingName,
		"/run/flatpak/" + missingName,
		"/.flatpak-info",
		"/.flatpak-info/" + missingName,
		"/proc/self/fd/3",
		"/proc/self/fd/987", // surely closed: the list decides, not stat
		"/dev/fd/987",
		"/tmp/" + missingName,
		"/var/tmp/" + missingName,
	} {
		if r, target, _ := Route(c, file, []string{file}, false); r != Local {
			t.Errorf("%s: got host %q, want local", file, target)
		}
	}

	// The sandbox's own file always wins (this test runs where /bin/sh and
	// the dynamic loader exist, as they do in every runtime).
	for _, file := range []string{"/bin/sh", "/usr/bin/env", "/lib64/ld-linux-x86-64.so.2"} {
		if _, err := os.Stat(file); err != nil {
			continue
		}
		if r, _, _ := Route(c, file, []string{file, "--library-path", "/usr/lib", "/app/bin/x"}, false); r != Local {
			t.Errorf("%s exists here: got host", file)
		}
	}

	t.Run("shared tmp", func(t *testing.T) {
		c := c
		c.TmpShared = true
		file := "/tmp/" + missingName
		if r, target, _ := Route(c, file, []string{file}, false); r != Host || target != file {
			t.Fatalf("with --filesystem=/tmp: got %d %q", r, target)
		}
		file = "/var/tmp/" + missingName
		if r, _, _ := Route(c, file, []string{file}, false); r != Local {
			t.Fatal("--filesystem=/tmp does not share /var/tmp")
		}
	})

	// Private paths in the arguments or the cwd are not consulted for a
	// missing absolute target, as before: running it here could only fail
	// with ENOENT (compare `node /app/x.js` with node only on the host).
	// Only the classification of the target itself changed.
	t.Run("private arguments", func(t *testing.T) {
		for _, tc := range []struct {
			file, arg, cwd string
			want           int
		}{
			{"/home/user/.local/bin/" + missingName, "/app/x", testCwd, Host},
			{"/home/user/.local/bin/" + missingName, "x", "/app/share", Host},
			{"/usr/bin/" + missingName, "/app/x", testCwd, Host},
			{"/usr/bin/" + missingName, "--library-path=/usr/lib", testCwd, Host},
			{"/usr/bin/" + missingName, "x", "/app/share", Host},
			{"/app/bin/" + missingName, "/home/user/x", testCwd, Local},
		} {
			c := c
			c.Cwd = tc.cwd
			if r, _, _ := Route(c, tc.file, []string{tc.file, tc.arg}, false); r != tc.want {
				t.Errorf("%s %s in %s: got %d, want %d", tc.file, tc.arg, tc.cwd, r, tc.want)
			}
		}
	})
}

// TestSandboxOnlyMatchesHostEnv keeps the two "the host has no such path"
// lists in step: an exec target missing in the sandbox stays local exactly
// where the Go side drops an environment value pointing there
// (internal/sandbox HostEnv), both unlike the argument rule, which also
// covers /usr, /bin and /lib*. /dev/fd and /proc/self/fd are the documented
// difference: an exec of a closed fd stays local, an environment value
// naming one is passed on.
func TestSandboxOnlyMatchesHostEnv(t *testing.T) {
	fs := newFS(t, nil, nil)
	c := fs.ctx("")
	fdOnly := map[string]bool{"/dev/fd": true, "/proc/self/fd": true}
	for _, p := range []string{
		"/app", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/tmp", "/var/tmp",
		"/run/flatpak", "/run/host", "/.flatpak-info", "/dev/fd", "/proc/self/fd",
		"/etc", "/home", "/opt", "/var", "/run/user",
	} {
		file := p + "/" + missingName
		stays := func() bool { r, _, _ := Route(c, file, []string{file}, false); return r == Local }()
		dropped := len(sandbox.HostEnv([]string{"FPS_V=" + file}, false)) == 0
		if fdOnly[p] {
			if !stays || dropped {
				t.Errorf("%s: exec stays %v (want true), env dropped %v (want false)", p, stays, dropped)
			}
			continue
		}
		if stays != dropped {
			t.Errorf("%s: missing exec target stays local %v, environment value dropped %v", p, stays, dropped)
		}
		// Every sandbox-only location is also private for the argument rule.
		if stays && !MentionsPrivate(file, false) {
			t.Errorf("%s: sandbox-only for exec targets but not private in arguments", p)
		}
	}
}

func TestNegativeCache(t *testing.T) {
	fs := newFS(t, []string{"apptool"}, nil)
	c := fs.ctx("")
	cache := fs.cacheFor()
	now := time.Now()

	// First call asks the host; the answer is recorded as the command would.
	r, _, lookups := Route(c, "apptool", []string{"apptool"}, true)
	if r != Local || lookups != 1 {
		t.Fatalf("first call: got %d after %d lookups", r, lookups)
	}
	cache.Record("apptool", false, "gen1")
	cache.Record("nosuch", false, "gen1")

	age := func(name string, seconds int64) {
		t.Helper()
		ts := now.Add(-time.Duration(seconds) * time.Second)
		if err := os.Chtimes(filepath.Join(c.CacheNone, name), ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	c.Now = now.Unix()

	for _, tc := range []struct {
		name    string
		age     int64
		lookups int
	}{
		{"apptool", NegTTLLocal - 1, 0},  // sandbox has it: trusted for a minute
		{"apptool", NegTTLLocal + 1, 1},  // then asked again
		{"nosuch", NegTTLMissing - 1, 0}, // nobody has it: trusted briefly
		{"nosuch", NegTTLMissing + 1, 1}, // so a new host command shows up soon
		{"nosuch", -10, 1},               // a future mtime is not trusted
	} {
		cache.Record(tc.name, false, "gen1")
		age(tc.name, tc.age)
		r, _, lookups := Route(c, tc.name, []string{tc.name}, true)
		if r != Local || lookups != tc.lookups {
			t.Errorf("%s aged %ds: got route %d after %d lookups, want local after %d",
				tc.name, tc.age, r, lookups, tc.lookups)
		}
	}

	// A positive entry routes to the host without asking.
	cache.Record("git", true, "gen1")
	if r, target, lookups := Route(c, "git", []string{"git"}, true); r != Host || target != "git" || lookups != 0 {
		t.Fatalf("cached host name: got %d %q after %d lookups", r, target, lookups)
	}

	// A new host generation (a host PATH directory changed) drops the cache.
	cache.Record("other", true, "gen2")
	if got := CacheState(c.CacheHost, c.CacheNone, c.Now, "git", false); got != CacheUnknown {
		t.Fatalf("after a generation change git is %d, want unknown", got)
	}
	if got := CacheState(c.CacheHost, c.CacheNone, c.Now, "apptool", true); got != CacheUnknown {
		t.Fatalf("after a generation change apptool is %d, want unknown", got)
	}
	if got := CacheState(c.CacheHost, c.CacheNone, c.Now, "other", false); got != CacheHost {
		t.Fatalf("the entry that brought the new generation is %d", got)
	}

	// No cache directory: every unknown name is asked.
	c.CacheHost, c.CacheNone = "", ""
	if _, _, lookups := Route(c, "apptool", []string{"apptool"}, true); lookups != 1 {
		t.Fatal("without a cache the host is asked")
	}
}

func TestInvalidNamesNeverCached(t *testing.T) {
	fs := newFS(t, nil, nil)
	for _, n := range []string{"", ".", "..", "a/b"} {
		if got := CacheState(filepath.Join(fs.cache, "host"), filepath.Join(fs.cache, "none"), 0, n, false); got != CacheUnknown {
			t.Errorf("%q: %d", n, got)
		}
	}
}

func TestCwdKeyMatchesGo(t *testing.T) {
	for _, cwd := range []string{"/", "/home/user/project", "/home/user/プロジェクト", "/a b/c", ""} {
		if c, g := CwdKey(cwd), agent.Key(cwd); c != g {
			t.Errorf("%q: C %s, Go %s", cwd, c, g)
		}
	}
}

// TestShimDir: a file in the shim directory is routed as its bare name,
// whichever way it was reached, and a shim never counts as the sandbox
// having the command (the preload side of DESIGN.md, shim).
func TestShimDir(t *testing.T) {
	fs := newFS(t, nil, []string{"both"})
	shims := filepath.Join(filepath.Dir(fs.appBin), "shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"nvim", "both"} {
		if err := os.WriteFile(filepath.Join(shims, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := fs.ctx("nvim:both")
	c.Path = shims + ":" + c.Path
	c.ShimDir = shims + "/" // trailing slash: the same directory
	shim := filepath.Join(shims, "nvim")
	for _, tc := range []struct {
		name, file string
		argv       []string
		search     bool
		want       int
		target     string
	}{
		{"bare name", "nvim", []string{"nvim"}, true, Host, "nvim"},
		{"explicit shim path", shim, []string{shim, "--version"}, false, Host, "nvim"},
		{"PATH candidate", shim, []string{"nvim"}, false, Host, "nvim"},
		{"shim is not a local copy: private argument still forwards", "nvim", []string{"nvim", "/app/x"}, true, Host, "nvim"},
		{"shim path, private argument, host only", shim, []string{shim, "/app/x"}, false, Host, "nvim"},
		{"sandbox has it and the argument is private", filepath.Join(shims, "both"), []string{"both", "/app/x"}, false, Local, ""},
	} {
		r, target, _ := Route(c, tc.file, tc.argv, tc.search)
		if r != tc.want || target != tc.target {
			t.Errorf("%s: got %d %q, want %d %q", tc.name, r, target, tc.want, tc.target)
		}
	}
	lc := c
	lc.Local = "nvim"
	if r, _, lookups := Route(lc, shim, []string{shim}, false); r != Local || lookups != 0 {
		t.Errorf("FLATPARK_SPAWN_LOCAL: got %d after %d lookups", r, lookups)
	}
	// Not the shim directory: an existing file stays an explicit choice.
	nc := c
	nc.ShimDir = ""
	if r, _, _ := Route(nc, shim, []string{shim}, false); r != Local {
		t.Error("without FLATPARK_SPAWN_SHIMS an existing absolute path stays local")
	}
	nc.ShimDir = "relative/shims"
	if r, _, _ := Route(nc, shim, []string{shim}, false); r != Local {
		t.Error("a relative FLATPARK_SPAWN_SHIMS is ignored")
	}
}

// TestPrivateRuleMatchesC: the shim mode's Go copy of the argument rule
// (internal/sandbox) decides as preload/route.c does, on a fixed list and
// on random strings built from the characters that matter.
func TestPrivateRuleMatchesC(t *testing.T) {
	corpus := []string{"", "/", "/app", "/app/", "/apps", "x/app", "/usr/lib", "-I/usr/include", "--prefix=/app",
		"PATH=/usr/bin:$PATH", "(cd /tmp && ls)", "cat '/app/x'", "`/bin/x`", "{/lib}", "a,/lib64", "/lib32;x",
		"/run/flatpak/bus", "/run/hostx", "/.flatpak-info", "/.flatpak-infox", "/dev/fd/63", "/proc/self/fd/3",
		"/proc/self/fdx", "/var/tmp", "/var/tmpx", "https://example.com/app/x", "@/sbin", "x</tmp>", "/tmp|y", "/etc/hosts"}
	pieces := []string{"/app", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32", "/tmp", "/var/tmp", "/run/flatpak",
		"/run/host", "/.flatpak-info", "/dev/fd", "/proc/self/fd", "/", "x", " ", "\t", "'", "\"", "=", ":", ";", "(", ")",
		"`", ",", "<", ">", "|", "&", "@", "{", "}", "-", "a", "\n"}
	seed := uint64(1)
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for k := next(6); k >= 0; k-- {
			b.WriteString(pieces[next(len(pieces))])
		}
		corpus = append(corpus, b.String())
	}
	for _, s := range corpus {
		for _, shared := range []bool{false, true} {
			if c, g := MentionsPrivate(s, shared), sandbox.MentionsPrivate(s, shared); c != g {
				t.Fatalf("mentions %q (tmp shared %v): C %v, Go %v", s, shared, c, g)
			}
			if c, g := IsPrivate(s, shared), sandbox.IsPrivatePath(s, shared); c != g {
				t.Fatalf("is private %q (tmp shared %v): C %v, Go %v", s, shared, c, g)
			}
		}
	}
}
