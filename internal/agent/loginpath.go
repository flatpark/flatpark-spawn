// Package agent is the host side. The sandbox sends the flatpark-spawn
// binary itself through HostCommand (/proc/self/fd/4) and it runs here as
// the agent: it finds the command in the user's login-shell PATH, execs it,
// or reports that the host has no such command.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/names"
)

// RefreshMin is the shortest interval between two login-shell runs. A miss
// refreshes the cached PATH, but a burst of misses (an app probing for
// several commands at start) pays for one shell, not one per name.
const RefreshMin = 2 * time.Second

// MaxAge is how long a cached login PATH is used before the next agent run
// takes it again, so that a change that adds no command to a directory
// already on it (`mise use node@22` switching directories) is picked up.
const MaxAge = 60 * time.Second

// ShellTimeout bounds a login shell that hangs in its rc files.
const ShellTimeout = 10 * time.Second

// pathMarker precedes the PATH in the login shell's output. The script
// prints it in two pieces so that a shell echoing its input (xtrace,
// verbose) cannot produce a false match.
const pathMarker = "__FPS_LOGIN_PATH="

// MaxEntries bounds the per-directory cache; the oldest entries go first.
const MaxEntries = 256

// LoginPath caches the PATH of the user's login shell, which is what the
// user's terminal sees (mise, nvm, ~/.cargo/bin ...). The session helper
// that HostCommand runs under only has the systemd user environment.
//
// The PATH depends on the directory: mise, asdf, direnv and nvm's auto-use
// change it per project (a project pinning node@20 puts that node first).
// So the shell runs in the command's working directory, and the result is
// cached per directory.
type LoginPath struct {
	// File is the cache entry for Cwd, on the host:
	// $XDG_RUNTIME_DIR/flatpark-spawn/login-path.d/<fnv64 of Cwd>.
	File string
	// Cwd is the directory the PATH is for, and the login shell runs in.
	Cwd string
	// Shell is the user's login shell.
	Shell string
	// Fallback is used when the shell gives nothing: the session PATH.
	Fallback string
	// Now and Run are replaced in tests.
	Now func() time.Time
	Run func(ctx context.Context, shell, cwd string) ([]byte, error)
}

// Key names a directory in the caches, on both sides (preload/route.c's
// fps_cwd_key computes the same): FNV-64a of the path, in hex.
func Key(cwd string) string {
	h := fnv.New64a()
	h.Write([]byte(cwd))
	return fmt.Sprintf("%016x", h.Sum64())
}

// NewLoginPath fills in the defaults from the host environment.
func NewLoginPath(env func(string) string, cacheDir, cwd string) *LoginPath {
	return &LoginPath{
		File:     filepath.Join(cacheDir, names.HostPathCache, Key(cwd)),
		Cwd:      cwd,
		Shell:    UserShell(env),
		Fallback: env("PATH"),
		Now:      time.Now,
		Run:      RunLoginShell,
	}
}

// Cached returns the cached PATH and when it was taken. The entry names its
// directory, so a hash collision reads as a miss.
func (l *LoginPath) Cached() (string, time.Time, bool) {
	b, err := os.ReadFile(l.File)
	if err != nil {
		return "", time.Time{}, false
	}
	lines := strings.SplitN(string(b), "\n", 4)
	if len(lines) < 3 || lines[0] != l.Cwd || lines[1] == "" {
		return "", time.Time{}, false
	}
	ns, err := strconv.ParseInt(strings.TrimSpace(lines[2]), 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return lines[1], time.Unix(0, ns), true
}

// Refresh runs the login shell and stores its PATH.
func (l *LoginPath) Refresh() string {
	ctx, cancel := context.WithTimeout(context.Background(), ShellTimeout)
	defer cancel()
	path := ""
	if out, err := l.Run(ctx, l.Shell, l.Cwd); err == nil {
		path, _ = ParseShellOutput(out)
	}
	if path == "" {
		path = l.Fallback
	}
	l.store(path)
	return path
}

func (l *LoginPath) store(path string) {
	dir := filepath.Dir(l.File)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_, statErr := os.Stat(l.File)
	tmp := fmt.Sprintf("%s.%d.tmp", l.File, os.Getpid())
	data := fmt.Sprintf("%s\n%s\n%d\n", l.Cwd, path, l.Now().UnixNano())
	if os.WriteFile(tmp, []byte(data), 0o600) == nil {
		_ = os.Rename(tmp, l.File)
	}
	if statErr != nil {
		Prune(dir, MaxEntries)
	}
}

// Prune keeps the newest max entries of a cache directory (by mtime).
func Prune(dir string, max int) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= max {
		return
	}
	type aged struct {
		name string
		t    time.Time
	}
	list := make([]aged, 0, len(entries))
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			list = append(list, aged{e.Name(), info.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.After(list[j].t) })
	for _, a := range list[min(max, len(list)):] {
		_ = os.RemoveAll(filepath.Join(dir, a.name))
	}
}

// Get returns the PATH, running the shell when nothing is cached or the
// cache is older than MaxAge.
func (l *LoginPath) Get() string {
	p, _, _ := l.get()
	return p
}

func (l *LoginPath) get() (path string, at time.Time, refreshed bool) {
	if p, at, ok := l.Cached(); ok && l.Now().Sub(at) < MaxAge {
		return p, at, false
	}
	return l.Refresh(), l.Now(), true
}

// Resolve finds name. A miss refreshes the login PATH once (unless that
// just happened) so that a tool installed after the cache was filled, in a
// directory the cached PATH does not have yet, is found. It returns the
// executable, the PATH it was looked up in, and whether it was found.
func (l *LoginPath) Resolve(name string) (string, string, bool) {
	path, at, refreshed := l.get()
	if full, ok := LookPath(name, path); ok {
		return full, path, true
	}
	if !refreshed && l.Now().Sub(at) >= RefreshMin {
		path = l.Refresh()
		if full, ok := LookPath(name, path); ok {
			return full, path, true
		}
	}
	return "", path, false
}

// LookPath searches path for an executable regular file. Relative entries
// are skipped: the agent's working directory is the caller's.
func LookPath(name, path string) (string, bool) {
	if name == "" || strings.ContainsRune(name, '/') {
		return "", false
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		full := filepath.Join(dir, name)
		if IsExecutable(full) {
			return full, true
		}
	}
	return "", false
}

// IsExecutable reports an executable regular file.
func IsExecutable(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && unix.Access(path, unix.X_OK) == nil
}

// Generation fingerprints a PATH and the modification times of its
// directories. Installing or removing a command changes it.
func Generation(path string) string {
	h := fnv.New64a()
	h.Write([]byte(path))
	for _, dir := range filepath.SplitList(path) {
		var st unix.Stat_t
		if dir == "" || unix.Stat(dir, &st) != nil {
			continue
		}
		fmt.Fprintf(h, "\n%s %d.%d", dir, st.Mtim.Sec, st.Mtim.Nsec)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// ParseShellOutput picks the PATH out of whatever the rc files printed.
func ParseShellOutput(out []byte) (string, bool) {
	found := ""
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, pathMarker); i >= 0 {
			found = strings.TrimRight(line[i+len(pathMarker):], "\r")
		}
	}
	return found, found != ""
}

// ShellArgs is the login shell's command line.
func ShellArgs(shell string) []string {
	head, tail := pathMarker[:len(pathMarker)-1], pathMarker[len(pathMarker)-1:]
	script := fmt.Sprintf(`printf '%%s%%s\n' '%s' "%s$PATH"`, head, tail)
	if filepath.Base(shell) == "fish" {
		script = fmt.Sprintf(`printf '%%s%%s\n' '%s' "%s"(string join : $PATH)`, head, tail)
	}
	return []string{shell, "-l", "-i", "-c", script}
}

// RunLoginShell runs the shell in cwd (with PWD set, which is what mise and
// direnv look at), in a session of its own (no controlling terminal for rc
// files to write to), stdin from /dev/null, stderr dropped, stdout
// captured: nothing it prints reaches the caller.
func RunLoginShell(ctx context.Context, shell, cwd string) ([]byte, error) {
	if shell == "" {
		return nil, errors.New("no login shell")
	}
	args := ShellArgs(shell)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(withoutVar(os.Environ(), "PWD"), "TERM=dumb", "PWD="+cwd)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	// A daemon started from an rc file may keep stdout open; do not wait
	// for it once the shell itself has exited.
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if err != nil && out.Len() == 0 {
			return nil, err
		}
		return out.Bytes(), nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return out.Bytes(), ctx.Err()
	}
}

func withoutVar(env []string, name string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// UserShell is $SHELL, else the passwd entry, else /bin/sh. The session
// helper's environment often has no SHELL.
func UserShell(env func(string) string) string {
	if s := env("SHELL"); s != "" {
		return s
	}
	if s := passwdShell("/etc/passwd", os.Getuid()); s != "" {
		return s
	}
	return "/bin/sh"
}

func passwdShell(file string, uid int) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	want := strconv.Itoa(uid)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) >= 7 && f[2] == want {
			return f[6]
		}
	}
	return ""
}
