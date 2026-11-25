//go:build linux

package preloadtest

import (
	"bufio"
	"bytes"
	"debug/elf"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// world is a built library with its fixtures: mount stands in for
// /app/lib/flatpark-spawn (mount/bin/flatpark-spawn is the fake command
// below), lib is mount/lib/libflatpark-spawn.so, launch the fixture that
// starts a program through one exec entry point, shims a shim directory
// and sbin a directory standing in for the sandbox's own /usr/bin.
type world struct {
	dir, mount, lib, launch, helper, shims, sbin string
}

var (
	built    world
	buildErr error
	once     sync.Once
)

// repo is the repository root, from this package's directory.
const repo = "../.."

func readNames(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, "names.mk"))
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{}
	line := regexp.MustCompile(`^([A-Z_]+)\s*:=\s*(.*?)\s*$`)
	ref := regexp.MustCompile(`\$\(([A-Z_]+)\)`)
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if m := line.FindStringSubmatch(sc.Text()); m != nil {
			vars[m[1]] = ref.ReplaceAllStringFunc(m[2], func(r string) string { return vars[r[2:len(r)-1]] })
		}
	}
	return vars
}

func run(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %v\n%s", name, args, err, out)
	}
	return nil
}

// copySources reads the library's sources through Go, so that a change to
// them invalidates go test's cache (a file read only by cc is not seen).
func copySources(dst string) error {
	for _, f := range []string{"../../preload/preload.c", "../../preload/route.c", "../../preload/route.h", "testdata/launch.c"} {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.Base(f)), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// fakeHost is the flatpark-spawn command for these tests. The "host" has
// fps-hostonly and fps-both; a forwarded exec prints what it got. Run
// under any other name (a shim), it says so: that is the shim mode taking
// over, which these tests only need to tell apart from a forward.
const fakeHost = `#!/bin/sh
case ${0##*/} in
flatpark-spawn) ;;
*) echo "shim: ${0##*/} $*"; exit 0 ;;
esac
case $1 in
--x-lookup) case $2 in fps-hostonly|fps-both) exit 0 ;; esac; exit 1 ;;
--x-exec) t=$2; shift 3; echo "host: $t argc=$#"; exit 0 ;;
esac
exit 2
`

func setup(t *testing.T) world {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no cc")
	}
	vars := readNames(t)
	once.Do(func() {
		dir, err := os.MkdirTemp("", "fps-preloadtest-")
		if err != nil {
			buildErr = err
			return
		}
		w := world{dir: dir, mount: filepath.Join(dir, "mnt")}
		w.lib = filepath.Join(w.mount, "lib", vars["LIB"])
		w.helper = filepath.Join(w.mount, "bin", vars["NAME"])
		w.launch = filepath.Join(dir, "launch")
		w.shims = filepath.Join(dir, "shims")
		w.sbin = filepath.Join(dir, "sbin")
		src, gen := filepath.Join(dir, "src"), filepath.Join(dir, "gen")
		for _, d := range []string{src, gen, filepath.Join(w.mount, "lib"), filepath.Join(w.mount, "bin"), w.shims, w.sbin} {
			if buildErr = os.MkdirAll(d, 0o755); buildErr != nil {
				return
			}
		}
		if buildErr = copySources(src); buildErr != nil {
			return
		}
		env := os.Environ()
		for k, v := range vars {
			env = append(env, k+"="+v)
		}
		env = append(env, "MOUNT="+w.mount)
		cmd := exec.Command("sh", filepath.Join(repo, "scripts", "gen-names.sh"), "c")
		cmd.Env = env
		h, err := cmd.Output()
		if err != nil {
			buildErr = fmt.Errorf("gen-names.sh: %v", err)
			return
		}
		if buildErr = os.WriteFile(filepath.Join(gen, "names.h"), h, 0o644); buildErr != nil {
			return
		}
		// The Makefile's flags.
		flags := []string{"-std=c11", "-O2", "-fPIC", "-Wall", "-Wextra", "-Werror", "-fvisibility=hidden",
			"-D_FORTIFY_SOURCE=2", "-I" + gen, "-I" + src, "-shared", "-Wl,-z,relro,-z,now", "-Wl,--as-needed",
			"-o", w.lib, filepath.Join(src, "preload.c"), filepath.Join(src, "route.c")}
		for _, step := range [][]string{
			flags,
			{"-O2", "-pthread", "-o", w.launch, filepath.Join(src, "launch.c")},
		} {
			if buildErr = run(dir, cc, step...); buildErr != nil {
				return
			}
		}
		if buildErr = os.WriteFile(w.helper, []byte(fakeHost), 0o755); buildErr != nil {
			return
		}
		// Shims point at the command, as --x-shims makes them. fps-both
		// also exists in the "sandbox" (sbin).
		for _, n := range []string{"fps-hostonly", "fps-both"} {
			if buildErr = os.Symlink(w.helper, filepath.Join(w.shims, n)); buildErr != nil {
				return
			}
		}
		buildErr = os.WriteFile(filepath.Join(w.sbin, "fps-both"), []byte("#!/bin/sh\necho \"local: fps-both $*\"\n"), 0o755)
		built = w
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return built
}

func TestMain(m *testing.M) {
	code := m.Run()
	if os.Getenv("FPS_KEEP") != "" {
		fmt.Println("kept", built.dir)
		os.Exit(code)
	}
	if built.dir != "" {
		os.RemoveAll(built.dir)
	}
	os.Exit(code)
}

// env: our library preloaded and enabled, PATH as given.
func (w world) env(path string, extra ...string) []string {
	e := []string{"PATH=" + path, "HOME=" + w.dir, "LC_ALL=C", "LD_PRELOAD=" + w.lib, "FLATPARK_SPAWN_ENABLE=1"}
	return append(e, extra...)
}

func (w world) start(t *testing.T, env []string, mode string, child ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(w.launch, append([]string{mode}, child...)...)
	cmd.Env = env
	// Not w.dir: a working directory under /tmp is sandbox-private and
	// would keep a command the sandbox has local.
	cmd.Dir = "/"
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

var modes = []string{"execve", "execv", "execvp", "execvpe", "execl", "execle", "execlp", "spawn", "spawnp", "fork"}

// TestForwardsEveryEntry: a name only the host has is forwarded from every
// hooked entry point, including the child of a fork taken while another
// thread holds the allocator busy (the allocator-free exec path).
func TestForwardsEveryEntry(t *testing.T) {
	w := setup(t)
	for _, mode := range modes {
		out, errs, code := w.start(t, w.env("/usr/bin:/bin"), mode, "fps-hostonly")
		if want := "host: fps-hostonly argc=0\n"; out != want || code != 0 {
			t.Errorf("%s: got exit %d %q (stderr: %s), want %q", mode, code, out, errs, want)
		}
	}
}

// TestLargeArgsAndEnvironment: beyond the stack budget the forwarding
// arrays come from mmap; the result is the same.
func TestLargeArgsAndEnvironment(t *testing.T) {
	w := setup(t)
	extra := make([]string, 10000)
	for i := range extra {
		extra[i] = fmt.Sprintf("FPS_FILL_%d=%s", i, strings.Repeat("x", 20))
	}
	args := make([]string, 9000)
	for i := range args {
		args[i] = "a"
	}
	for _, mode := range []string{"execve", "spawn", "fork"} {
		out, errs, code := w.start(t, w.env("/usr/bin:/bin", extra...), mode, append([]string{"fps-hostonly"}, args...)...)
		if want := fmt.Sprintf("host: fps-hostonly argc=%d\n", len(args)); out != want || code != 0 {
			t.Errorf("%s: got exit %d %.300q (stderr: %.300s), want %q", mode, code, out, errs, want)
		}
	}
}

// TestShims: an exec of a file in the shim directory (FLATPARK_SPAWN_SHIMS)
// is routed as the bare name, without running the shim; a shim does not
// count as the sandbox having the command. Where the route is local, the
// shim runs (its shim mode finds the sandbox's copy).
func TestShims(t *testing.T) {
	w := setup(t)
	path := w.shims + ":" + w.sbin + ":/usr/bin:/bin"
	env := w.env(path, "FLATPARK_SPAWN_SHIMS="+w.shims)
	shim := filepath.Join(w.shims, "fps-hostonly")
	for _, tc := range []struct {
		name  string
		env   []string
		mode  string
		child []string
		want  string
	}{
		{"execvp finds the shim first", env, "execvp", []string{"fps-hostonly"}, "host: fps-hostonly argc=0"},
		{"explicit path into the shim directory (findNvim)", env, "execve", []string{shim, "--version"}, "host: fps-hostonly argc=1"},
		{"explicit path, posix_spawn", env, "spawn", []string{shim}, "host: fps-hostonly argc=0"},
		{"explicit path, fork in a threaded process", env, "fork", []string{shim}, "host: fps-hostonly argc=0"},
		{"a shim is not the sandbox's own copy: private argument, host only", env, "execvp",
			[]string{"fps-hostonly", "/app/x"}, "host: fps-hostonly argc=1"},
		{"private argument, the sandbox has it: local, through the shim", env, "execvp",
			[]string{"fps-both", "/app/x"}, "shim: fps-both /app/x"},
		{"FLATPARK_SPAWN_LOCAL: local, through the shim", append(env[:len(env):len(env)], "FLATPARK_SPAWN_LOCAL=fps-hostonly"),
			"execvp", []string{"fps-hostonly"}, "shim: fps-hostonly "},
		{"without FLATPARK_SPAWN_SHIMS a shim is an ordinary file", w.env(path), "execve", []string{shim, "x"}, "shim: fps-hostonly x"},
	} {
		out, errs, code := w.start(t, tc.env, tc.mode, tc.child...)
		if out != tc.want+"\n" || code != 0 {
			t.Errorf("%s: got exit %d %q (stderr: %s), want %q", tc.name, code, out, errs, tc.want)
		}
	}
}

// TestNoHeapOrStdioImports guards the async-signal-safety of the exec path
// (DESIGN.md): the library must not import the allocator, stdio, getenv or
// the file actions that copy paths. What is left and not async-signal-safe
// is listed with where it runs.
func TestNoHeapOrStdioImports(t *testing.T) {
	w := setup(t)
	f, err := elf.Open(w.lib)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	syms, err := f.ImportedSymbols()
	if err != nil {
		t.Fatal(err)
	}
	forbidden := regexp.MustCompile(`^(malloc|calloc|realloc|reallocarray|free|strdup|strndup|asprintf|` +
		`.*printf.*|getenv|secure_getenv|opendir|getcwd|posix_spawn_file_actions_addopen|qsort|` +
		`fopen|fclose|fgets|fputs|fwrite|perror|strerror)$`)
	allowed := map[string]string{
		"dlsym":                            "constructor (resolve); afterwards only when a pointer is still NULL",
		"setenv":                           "constructor only",
		"posix_spawn_file_actions_init":    "socket delegation, not on the exec path",
		"posix_spawn_file_actions_adddup2": "socket delegation, not on the exec path",
		"posix_spawn_file_actions_destroy": "socket delegation, not on the exec path",
	}
	for _, s := range syms {
		if forbidden.MatchString(s.Name) {
			t.Errorf("imports %s", s.Name)
		} else if why, ok := allowed[s.Name]; ok {
			t.Logf("imports %s: %s", s.Name, why)
		}
	}
}
