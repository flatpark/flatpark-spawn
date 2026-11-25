package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/config"
	"github.com/flatpark/flatpark-spawn/internal/names"

	"github.com/flatpark/flatpark-spawn/internal/proto"
	"github.com/flatpark/flatpark-spawn/internal/pty"
)

func writeExe(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseShellOutput(t *testing.T) {
	out := "Welcome!\n\x1b[1mprompt\x1b[0m\n" + pathMarker + "/a:/b\r\nbye\n"
	got, ok := ParseShellOutput([]byte(out))
	if !ok || got != "/a:/b" {
		t.Fatalf("got %q %v", got, ok)
	}
	// The last marker wins (an rc file that runs a subshell printing PATH).
	got, _ = ParseShellOutput([]byte(pathMarker + "/old\n" + pathMarker + "/new\n"))
	if got != "/new" {
		t.Fatalf("got %q", got)
	}
	if _, ok := ParseShellOutput([]byte("no marker\n")); ok {
		t.Fatal("no marker must not parse")
	}
}

func TestShellArgsCannotEchoTheMarker(t *testing.T) {
	for _, sh := range []string{"/bin/zsh", "/usr/bin/fish"} {
		args := ShellArgs(sh)
		if args[0] != sh || args[1] != "-l" || args[2] != "-i" || args[3] != "-c" {
			t.Fatalf("%v", args)
		}
		if strings.Contains(args[4], pathMarker) {
			t.Fatalf("%s: the script contains the marker verbatim: %s", sh, args[4])
		}
	}
	// And it really prints it, in a POSIX shell.
	out, err := exec.Command("/bin/sh", "-c", ShellArgs("/bin/sh")[4]).Output()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := ParseShellOutput(out); !ok || p != os.Getenv("PATH") {
		t.Fatalf("got %q from %q", p, out)
	}
}

func TestRunLoginShellKeepsOutputAway(t *testing.T) {
	dir := t.TempDir()
	// Stands in for `zsh -l -i -c SCRIPT`: rc files print and change PATH.
	shell := writeExe(t, dir, "noisy-sh", "#!/bin/sh\necho 'rc noise'\necho 'rc error' >&2\nPATH=/login/bin:$PATH\nshift 3\nexec /bin/sh -c \"$1\"\n")
	out, err := RunLoginShell(context.Background(), shell, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ParseShellOutput(out)
	if !ok || !strings.HasPrefix(p, "/login/bin:") {
		t.Fatalf("got %q from %q", p, out)
	}
	if strings.Contains(string(out), "rc error") {
		t.Fatal("stderr must be dropped")
	}
}

func TestRunLoginShellTimeout(t *testing.T) {
	dir := t.TempDir()
	shell := writeExe(t, dir, "hang-sh", "#!/bin/sh\nsleep 30\n")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := RunLoginShell(ctx, shell, t.TempDir()); err == nil {
		t.Fatal("want a timeout error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the hanging shell was not killed")
	}
}

func TestLookPath(t *testing.T) {
	dir := t.TempDir()
	writeExe(t, dir, "tool", "#!/bin/sh\n")
	if err := os.WriteFile(filepath.Join(dir, "data"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := "relative:" + "/nonexistent:" + dir
	if p, ok := LookPath("tool", path); !ok || p != filepath.Join(dir, "tool") {
		t.Fatalf("got %q %v", p, ok)
	}
	for _, n := range []string{"data", "subdir", "missing", "", "a/tool"} {
		if _, ok := LookPath(n, path); ok {
			t.Errorf("%q must not be found", n)
		}
	}
}

// fakeShell counts login-shell runs and reports a PATH the test controls.
type fakeShell struct {
	runs int
	path string
}

func (f *fakeShell) run(context.Context, string, string) ([]byte, error) {
	f.runs++
	return []byte("noise\n" + pathMarker + f.path + "\n"), nil
}

func TestResolveRefreshesOnMiss(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old")
	newDir := filepath.Join(root, "mise-new")
	writeExe(t, oldDir, "git", "#!/bin/sh\n")
	sh := &fakeShell{path: oldDir}
	now := time.Unix(1_800_000_000, 0)
	lp := &LoginPath{
		File:     filepath.Join(root, "cache", "login-path"),
		Cwd:      "/project",
		Shell:    "/bin/zsh",
		Fallback: "/usr/bin",
		Now:      func() time.Time { return now },
		Run:      sh.run,
	}

	if _, _, ok := lp.Resolve("git"); !ok || sh.runs != 1 {
		t.Fatalf("first lookup fills the cache: ok=%v runs=%d", ok, sh.runs)
	}
	if _, _, ok := lp.Resolve("git"); !ok || sh.runs != 1 {
		t.Fatalf("a hit does not run the shell: runs=%d", sh.runs)
	}

	// A tool appears in a directory the cached PATH lacks (new mise install).
	writeExe(t, newDir, "node", "#!/bin/sh\n")
	sh.path = oldDir + ":" + newDir
	if _, _, ok := lp.Resolve("node"); ok || sh.runs != 1 {
		t.Fatalf("within RefreshMin a miss does not run the shell again: runs=%d", sh.runs)
	}
	now = now.Add(RefreshMin)
	full, path, ok := lp.Resolve("node")
	if !ok || full != filepath.Join(newDir, "node") || sh.runs != 2 || path != sh.path {
		t.Fatalf("a miss refreshes: %q %q %v runs=%d", full, path, ok, sh.runs)
	}
	// The refreshed PATH is cached for the next agent.
	if p, _, ok := lp.Cached(); !ok || p != sh.path {
		t.Fatalf("cache holds %q", p)
	}

	// A new file in a directory already on PATH needs no refresh at all.
	writeExe(t, oldDir, "fresh", "#!/bin/sh\n")
	if _, _, ok := lp.Resolve("fresh"); !ok || sh.runs != 2 {
		t.Fatalf("new file in a known dir: ok=%v runs=%d", ok, sh.runs)
	}

	// Really missing: refreshed once, then answered from the cache.
	now = now.Add(RefreshMin)
	if _, _, ok := lp.Resolve("nosuch"); ok || sh.runs != 3 {
		t.Fatalf("missing: runs=%d", sh.runs)
	}
	if _, _, ok := lp.Resolve("nosuch"); ok || sh.runs != 3 {
		t.Fatalf("missing again within RefreshMin: runs=%d", sh.runs)
	}

	// Past MaxAge even a hit takes the PATH again (mise switched node's
	// directory; the old one still exists, so no miss would notice).
	switched := filepath.Join(root, "mise-switched")
	writeExe(t, switched, "node", "#!/bin/sh\n")
	sh.path = switched + ":" + oldDir
	now = now.Add(MaxAge)
	if full, _, ok := lp.Resolve("node"); !ok || full != filepath.Join(switched, "node") || sh.runs != 4 {
		t.Fatalf("after MaxAge: %q runs=%d", full, sh.runs)
	}
	if lp.Get() != sh.path || sh.runs != 4 {
		t.Fatalf("Get within MaxAge uses the cache: runs=%d", sh.runs)
	}
}

func TestRefreshFallsBackToSessionPath(t *testing.T) {
	root := t.TempDir()
	lp := &LoginPath{
		File:     filepath.Join(root, "login-path"),
		Cwd:      "/",
		Fallback: "/session/bin",
		Now:      time.Now,
		Run:      func(context.Context, string, string) ([]byte, error) { return []byte("garbage"), nil },
	}
	if got := lp.Refresh(); got != "/session/bin" {
		t.Fatalf("got %q", got)
	}
}

func TestGenerationChangesWhenADirectoryChanges(t *testing.T) {
	dir := t.TempDir()
	path := dir + ":/nonexistent"
	g1 := Generation(path)
	if g1 != Generation(path) {
		t.Fatal("generation must be stable")
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, past, past); err != nil {
		t.Fatal(err)
	}
	g2 := Generation(path)
	writeExe(t, dir, "newtool", "#!/bin/sh\n")
	if g3 := Generation(path); g3 == g2 {
		t.Fatal("adding a command must change the generation")
	}
	if Generation(path+":/other") == g1 {
		t.Fatal("a different PATH must change the generation")
	}
}

func TestUserShell(t *testing.T) {
	if got := UserShell(func(string) string { return "/bin/fish" }); got != "/bin/fish" {
		t.Fatal(got)
	}
	f := filepath.Join(t.TempDir(), "passwd")
	line := fmt.Sprintf("me:x:%d:%d::/home/me:/usr/bin/zsh\n", os.Getuid(), os.Getgid())
	if err := os.WriteFile(f, []byte("root:x:0:0::/root:/bin/bash\n"+line), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := passwdShell(f, os.Getuid()); got != "/usr/bin/zsh" && os.Getuid() != 0 {
		t.Fatal(got)
	}
}

func TestExecEnv(t *testing.T) {
	env := ExecEnv([]string{"PATH=/session", "HOME=/home/me", "FOO=1", "LANG=en_US.UTF-8"},
		map[string]string{"LANG": "C", "CI": "1", "PATH": "/app/bin"}, "/login:/session", "/bin/zsh")
	want := map[string]bool{"PATH=/login:/session": true, "HOME=/home/me": true, "FOO=1": true,
		"LANG=C": true, "CI=1": true, "SHELL=/bin/zsh": true}
	if len(env) != len(want) {
		t.Fatalf("%v", env)
	}
	for _, kv := range env {
		if !want[kv] {
			t.Fatalf("unexpected %q in %v", kv, env)
		}
	}
	env = ExecEnv([]string{"SHELL=/bin/bash"}, nil, "/p", "/bin/zsh")
	if len(env) != 2 || env[0] != "SHELL=/bin/bash" {
		t.Fatalf("the session's SHELL wins: %v", env)
	}
}

// The agent runs here as a child process with the control socket on fd 3,
// exactly as HostCommand sets it up on the host.
//
// No test may read the real ~/.config/flatpark-spawn/config.yaml: before
// anything runs, FLATPARK_SPAWN_CONFIG and XDG_CONFIG_HOME point into a
// temporary directory, for this process and every agent it starts.
func TestMain(m *testing.M) {
	if os.Getenv("FPS_TEST_AGENT") == "1" {
		os.Exit(Run(strings.Split(os.Getenv("FPS_TEST_ARGS"), "\x1f")))
	}
	dir, err := os.MkdirTemp("", "fps-agent-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv(names.Env("CONFIG"), filepath.Join(dir, "none", "config.yaml"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type agentResult struct {
	reply  proto.Reply
	status int
	stdout string
	stderr string
}

// agentRun describes one agent run.
type agentRun struct {
	args []string // after names.ModeAgent
	// loginPath, when set, is cached for seedCwd (fresh, so a miss does
	// not refresh it) and no real login shell runs. Empty: the agent runs
	// the shell (SHELL below) as it would on the host.
	loginPath string
	seedCwd   string
	shell     string            // default /bin/sh
	home      string            // HOME for the login shell; default unchanged
	runtime   string            // XDG_RUNTIME_DIR; default a temporary one
	overlay   map[string]string // the caller's environment
	ctty      *os.File          // passed as CttyFD (fd 5) when set
	config    string            // passed as ConfigArg when set
}

func runAgent(t *testing.T, path string, args ...string) agentResult {
	t.Helper()
	cwd := ""
	if len(args) > 1 {
		cwd = args[1]
	}
	return runAgentWith(t, agentRun{args: args, loginPath: path, seedCwd: cwd})
}

func runAgentWith(t *testing.T, r agentRun) agentResult {
	t.Helper()
	if r.runtime == "" {
		r.runtime = filepath.Join(t.TempDir(), "run")
	}
	if r.shell == "" {
		r.shell = "/bin/sh"
	}
	if r.loginPath != "" {
		cache := filepath.Join(r.runtime, names.Name, names.HostPathCache, Key(r.seedCwd))
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
			t.Fatal(err)
		}
		data := fmt.Sprintf("%s\n%s\n%d\n", r.seedCwd, r.loginPath, time.Now().UnixNano())
		if err := os.WriteFile(cache, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours := os.NewFile(uintptr(pair[0]), "ours")
	theirs := os.NewFile(uintptr(pair[1]), "theirs")
	defer ours.Close()
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()

	args := r.args
	if r.config != "" {
		args = append([]string{args[0], ConfigArg + r.config}, args[1:]...)
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "FPS_TEST_AGENT=1", "FPS_TEST_ARGS="+strings.Join(args, "\x1f"),
		"XDG_RUNTIME_DIR="+r.runtime, "SHELL="+r.shell, "ENV=")
	if r.home != "" {
		cmd.Env = append(cmd.Env, "HOME="+r.home)
	}
	cmd.ExtraFiles = []*os.File{theirs, self} // fds 3 and 4
	if r.ctty != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, r.ctty) // fd 5
	}
	// As under HostCommand: a session of its own, no controlling terminal
	// inherited from whoever runs the tests.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The caller's environment comes first on the control socket.
	if err := proto.Send(pair[0], "the environment", proto.EncodeEnv(r.overlay)); err != nil {
		t.Fatal(err)
	}
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	theirs.Close()
	b, recvErr := proto.Recv(pair[0])
	_ = cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	var rep proto.Reply
	if recvErr == nil {
		if rep, err = proto.Decode(b); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("no reply: %v", recvErr)
	}
	return agentResult{reply: rep, status: ws.ExitStatus(), stdout: out.String(), stderr: errb.String()}
}

func TestAgentTellsNotFoundFromExit127(t *testing.T) {
	bin := t.TempDir()
	writeExe(t, bin, "exits-127", "#!/bin/sh\necho ran\nexit 127\n")
	writeExe(t, bin, "says-hi", "#!/bin/sh\necho \"hi $1 $PWD\"\n")
	writeExe(t, bin, "show-overlay", "#!/bin/sh\necho \"$FPS_OVERLAY\"\n")
	writeExe(t, bin, "no-shebang", "echo plain script\n")
	cwd := t.TempDir()

	own := runAgent(t, bin, ModeExec, cwd, "exits-127", "exits-127")
	if own.reply.Kind != proto.Found || own.status != 127 || own.stdout != "ran\n" {
		t.Fatalf("a command's own 127: %+v", own)
	}
	missing := runAgent(t, bin, ModeExec, cwd, "nosuch-fps", "nosuch-fps")
	if missing.reply.Kind != proto.None || missing.stdout != "" {
		t.Fatalf("a missing command: %+v", missing)
	}
	ok := runAgent(t, bin, ModeExec, cwd, "says-hi", "says-hi", "there")
	if ok.reply.Kind != proto.Found || ok.status != 0 || ok.stdout != "hi there "+cwd+"\n" {
		t.Fatalf("argv and cwd: %+v", ok)
	}
	if ok.reply.Gen == "" || ok.reply.Gen != Generation(bin) {
		t.Fatalf("generation %q", ok.reply.Gen)
	}
	if ok.reply.Path != filepath.Join(bin, "says-hi") || ok.reply.LoginPath != bin || ok.reply.Shell != "/bin/sh" {
		t.Fatalf("what a direct start needs: %+v", ok.reply)
	}
	ov := runAgentWith(t, agentRun{args: []string{ModeExec, cwd, "show-overlay", "show-overlay"}, loginPath: bin, seedCwd: cwd,
		overlay: map[string]string{"FPS_OVERLAY": "from-sandbox"}})
	if ov.stdout != "from-sandbox\n" {
		t.Fatalf("the caller's environment: %+v", ov)
	}
	if ok.reply.Session == "" || proto.ParseHashes(ok.reply.Session)["SHELL"] != proto.HashValue("/bin/sh") {
		t.Fatalf("session fingerprint %q", ok.reply.Session)
	}
	if _, leaked := proto.ParseHashes(ok.reply.Session)["FPS_OVERLAY"]; leaked {
		t.Fatal("the overlay is not part of the session environment")
	}
	plain := runAgent(t, bin, ModeExec, cwd, "no-shebang", "no-shebang")
	if plain.status != 0 || plain.stdout != "plain script\n" {
		t.Fatalf("script without #!: %+v", plain)
	}
	abs := runAgent(t, bin, ModeExec, cwd, filepath.Join(bin, "says-hi"), "says-hi", "abs")
	if abs.reply.Kind != proto.Found || !strings.HasPrefix(abs.stdout, "hi abs") {
		t.Fatalf("absolute target: %+v", abs)
	}
	absMissing := runAgent(t, bin, ModeExec, cwd, "/nonexistent/tool", "tool")
	if absMissing.reply.Kind != proto.None {
		t.Fatalf("missing absolute target: %+v", absMissing)
	}
	nocwd := runAgent(t, bin, ModeExec, "/nonexistent-dir", "says-hi", "says-hi")
	if _, leaked := proto.ParseHashes(ok.reply.Session)["FPS_OVERLAY"]; leaked {
		t.Fatal("overlay leaked")
	}
	if nocwd.reply.Kind != proto.NoCwd || nocwd.reply.Path != "/nonexistent-dir" || nocwd.stdout != "" {
		t.Fatalf("missing cwd: %+v", nocwd)
	}
	found := runAgent(t, bin, ModeLookup, cwd, "says-hi")
	none := runAgent(t, bin, ModeLookup, cwd, "nosuch-fps")
	if gone := runAgent(t, bin, ModeLookup, "/nonexistent-dir", "says-hi"); gone.reply.Kind != proto.NoCwd {
		t.Fatalf("lookup in a missing directory: %+v", gone)
	}
	if found.reply.Kind != proto.Found || none.reply.Kind != proto.None || found.stdout != "" ||
		found.reply.Path != filepath.Join(bin, "says-hi") {
		t.Fatalf("lookup: %+v %+v", found, none)
	}
}

// The login PATH follows the directory, as mise/direnv make it: a temporary
// HOME whose .profile puts a different bin first depending on $PWD. Nothing
// is pre-seeded, so the agent runs the real (sh) login shell in each cwd.
func TestLoginPathPerDirectory(t *testing.T) {
	home := t.TempDir()
	writeExe(t, filepath.Join(home, "bin-a"), "tool", "#!/bin/sh\necho a\n")
	writeExe(t, filepath.Join(home, "bin-b"), "tool", "#!/bin/sh\necho b\n")
	writeExe(t, filepath.Join(home, "bin-b"), "only-b", "#!/bin/sh\necho only-b\n")
	profile := `case "$PWD" in
*/proj-a) PATH="$HOME/bin-a:$PATH" ;;
*/proj-b) PATH="$HOME/bin-b:$PATH" ;;
esac
export PATH
echo "profile noise"
`
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	projA, projB := filepath.Join(home, "proj-a"), filepath.Join(home, "proj-b")
	for _, d := range []string{projA, projB} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runtime := filepath.Join(t.TempDir(), "run")
	run := func(args ...string) agentResult {
		return runAgentWith(t, agentRun{args: args, home: home, runtime: runtime})
	}
	if r := run(ModeExec, projA, "tool", "tool"); r.stdout != "a\n" || r.reply.Kind != proto.Found {
		t.Fatalf("in proj-a: %+v", r)
	}
	if r := run(ModeExec, projB, "tool", "tool"); r.stdout != "b\n" {
		t.Fatalf("in proj-b: %+v", r)
	}
	// Cached per directory: still a, then b, with no shell run needed.
	if r := run(ModeExec, projA, "tool", "tool"); r.stdout != "a\n" {
		t.Fatalf("in proj-a again: %+v", r)
	}
	if r := run(ModeLookup, projB, "only-b"); r.reply.Kind != proto.Found || r.reply.Path != filepath.Join(home, "bin-b", "only-b") {
		t.Fatalf("only-b in proj-b: %+v", r)
	}
	if r := run(ModeLookup, projA, "only-b"); r.reply.Kind != proto.None {
		t.Fatalf("only-b is not on proj-a's PATH: %+v", r)
	}
	entries, _ := os.ReadDir(filepath.Join(runtime, names.Name, names.HostPathCache))
	if len(entries) != 2 {
		t.Fatalf("want one cache entry per directory, got %d", len(entries))
	}
	for _, d := range []string{projA, projB} {
		b, err := os.ReadFile(filepath.Join(runtime, names.Name, names.HostPathCache, Key(d)))
		if err != nil || !strings.HasPrefix(string(b), d+"\n") {
			t.Fatalf("entry for %s: %q %v", d, b, err)
		}
	}
}

func TestCachedEntryOfAnotherDirectoryIsAMiss(t *testing.T) {
	dir := t.TempDir()
	lp := &LoginPath{File: filepath.Join(dir, "entry"), Cwd: "/a", Now: time.Now}
	lp.store("/a-path")
	if p, _, ok := lp.Cached(); !ok || p != "/a-path" {
		t.Fatalf("%q %v", p, ok)
	}
	other := &LoginPath{File: lp.File, Cwd: "/b", Now: time.Now} // a hash collision
	if _, _, ok := other.Cached(); ok {
		t.Fatal("an entry written for /a must not answer for /b")
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		f := filepath.Join(dir, fmt.Sprint(i))
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(time.Duration(i) * time.Minute)
		_ = os.Chtimes(f, ts, ts)
	}
	Prune(dir, 3)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 || entries[0].Name() != "2" {
		t.Fatalf("kept %v", entries)
	}
}

// Messages near and over the control socket's limit: a long login PATH
// and a large caller environment go through; a reply that cannot fit is
// reported as such and the command is not started.
func TestLargeMessages(t *testing.T) {
	bin := t.TempDir()
	writeExe(t, bin, "show-big", "#!/bin/sh\necho ${#FPS_BIG}\n")
	cwd := t.TempDir()
	longPath := func(size int) string {
		var b strings.Builder
		for b.Len() < size {
			fmt.Fprintf(&b, "/nonexistent/fps-long-path-entry-%06d:", b.Len())
		}
		return b.String() + bin
	}
	big := strings.Repeat("x", 60<<10)

	r := runAgentWith(t, agentRun{args: []string{ModeExec, cwd, "show-big", "show-big"},
		loginPath: longPath(64 << 10), seedCwd: cwd, overlay: map[string]string{"FPS_BIG": big}})
	if r.reply.Kind != proto.Found || r.stdout != fmt.Sprintf("%d\n", len(big)) || len(r.reply.LoginPath) < 64<<10 {
		t.Fatalf("64 KiB PATH and 60 KiB environment: kind=%q stdout=%q", r.reply.Kind, r.stdout)
	}

	r = runAgentWith(t, agentRun{args: []string{ModeExec, cwd, "show-big", "show-big"},
		loginPath: longPath(200 << 10), seedCwd: cwd})
	if r.reply.Kind != proto.Fail || !strings.Contains(r.reply.Msg, "limit") || r.stdout != "" || r.status != 125 {
		t.Fatalf("200 KiB PATH: want an explicit error and no run, got %+v", r)
	}
}

// --ctty: the agent makes CttyFD the command's controlling terminal (stdin
// is a pipe here, as for `printf x | prog`, where the session helper does
// not set one up). Without the fd, --ctty is an error and nothing runs.
func TestAgentControllingTerminal(t *testing.T) {
	bin := t.TempDir()
	// The agent runs it with the login PATH, here just bin: absolute paths.
	writeExe(t, bin, "ttycheck", `#!/bin/sh
nr=$(/usr/bin/sed 's/^.*) //' /proc/$$/stat | /usr/bin/cut -d' ' -f5)
if (exec </dev/tty) 2>/dev/null; then dev=ok; else dev=fail; fi
echo "tty_nr_zero=$([ "$nr" = 0 ] && echo yes || echo no) devtty=$dev"
`)
	cwd := t.TempDir()
	p, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	defer p.Close()

	r := runAgentWith(t, agentRun{args: []string{ModeExec, CttyArg, cwd, "ttycheck", "ttycheck"},
		loginPath: bin, seedCwd: cwd, ctty: p.Slave()})
	if r.reply.Kind != proto.Found || r.stdout != "tty_nr_zero=no devtty=ok\n" {
		t.Fatalf("with --ctty: %+v", r)
	}
	r = runAgent(t, bin, ModeExec, cwd, "ttycheck", "ttycheck")
	if r.reply.Kind != proto.Found || r.stdout != "tty_nr_zero=yes devtty=fail\n" {
		t.Fatalf("without --ctty the command has no terminal here: %+v", r)
	}
	r = runAgentWith(t, agentRun{args: []string{ModeExec, CttyArg, cwd, "ttycheck", "ttycheck"},
		loginPath: bin, seedCwd: cwd})
	if r.reply.Kind != proto.Fail || r.stdout != "" {
		t.Fatalf("--ctty without the fd must fail and run nothing: %+v", r)
	}
}

// TestAgentConfig: the configuration decides where bare names are found,
// what PATH a command gets, which absolute targets the host "lacks", and
// is part of the generation.
func TestAgentConfig(t *testing.T) {
	login, extra, excl := t.TempDir(), t.TempDir(), t.TempDir()
	writeExe(t, login, "in-login", "#!/bin/sh\necho login \"$PATH\"\n")
	writeExe(t, extra, "in-extra", "#!/bin/sh\necho extra \"$PATH\"\n")
	writeExe(t, excl, "in-excl", "#!/bin/sh\necho excl\n")
	cwd := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) agentResult {
		t.Helper()
		return runAgentWith(t, agentRun{args: args, loginPath: login + ":" + excl, seedCwd: cwd, config: cfg})
	}

	// No file: 0.1, the login PATH unchanged.
	noFile := run(ModeExec, cwd, "in-login", "in-login")
	if noFile.stdout != "login "+login+":"+excl+"\n" || noFile.reply.Gen != Generation(login+":"+excl) {
		t.Fatalf("without a file: %+v", noFile)
	}

	write("include_path: [" + extra + "]\nexclude_path: [" + excl + "]\n")
	want := login + ":" + extra
	if r := run(ModeExec, cwd, "in-extra", "in-extra"); r.stdout != "extra "+want+"\n" {
		t.Errorf("include_path after the login PATH, exclude_path dropped: %+v", r)
	}
	if r := run(ModeLookup, cwd, "in-excl"); r.reply.Kind != proto.None {
		t.Errorf("a name only in an excluded directory: %+v", r.reply)
	}
	if r := run(ModeExec, cwd, filepath.Join(excl, "in-excl"), "in-excl"); r.reply.Kind != proto.None || r.stdout != "" {
		t.Errorf("an absolute target in an excluded directory: %+v", r)
	}
	if r := run(ModeExec, cwd, filepath.Join(extra, "in-extra"), "in-extra"); r.reply.Kind != proto.Found {
		t.Errorf("an absolute target elsewhere needs no include_path: %+v", r)
	}
	g1 := run(ModeLookup, cwd, "in-login").reply.Gen
	if g1 == Generation(want) || !strings.HasPrefix(g1, Generation(want)+".") {
		t.Errorf("generation %q carries the file's fingerprint", g1)
	}

	write("use_login_shell_path: false\ninclude_path: [" + extra + "]\n")
	if r := run(ModeLookup, cwd, "in-login"); r.reply.Kind != proto.None {
		t.Errorf("use_login_shell_path: false: %+v", r.reply)
	}
	only := run(ModeExec, cwd, "in-extra", "in-extra")
	if only.stdout != "extra "+extra+"\n" || only.reply.Gen == g1 {
		t.Errorf("only include_path, new generation: %+v (was %s)", only, g1)
	}

	write("include_paths: [" + extra + "]\n")
	broken := run(ModeExec, cwd, "in-login", "in-login")
	if broken.reply.Kind != proto.Found || broken.stdout != "login "+login+":"+excl+"\n" ||
		!strings.Contains(broken.stderr, cfg+":1: unknown key \"include_paths\" (did you mean include_path?); ignoring the configuration") {
		t.Errorf("a broken file is the defaults, with a warning: %+v", broken)
	}
	if strings.Count(broken.stderr, "\n") != 1 {
		t.Errorf("one warning per agent run: %q", broken.stderr)
	}
}

func TestAgentShimsAndCheck(t *testing.T) {
	login, extra := t.TempDir(), t.TempDir()
	writeExe(t, login, "nvim", "#!/bin/sh\n")
	cwd := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "config.yaml")
	report := func(mode string) config.Report {
		t.Helper()
		r := runAgentWith(t, agentRun{args: []string{mode, cwd}, loginPath: login, seedCwd: cwd, config: cfg})
		if r.reply.Kind != proto.Report || r.stderr != "" || r.stdout != "" {
			t.Fatalf("%s: %+v", mode, r)
		}
		rep, err := config.DecodeReport(r.reply.Msg)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	if r := report(ModeShims); r.Exists || len(r.Shims) != 0 {
		t.Fatalf("no file: %+v", r)
	}
	missing := filepath.Join(extra, "gone")
	if err := os.WriteFile(cfg, []byte("include_path: ["+missing+"]\nshims: [nvim, rg, nvim]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := report(ModeShims)
	if len(r.Shims) != 2 || r.Shims[0].Path != filepath.Join(login, "nvim") || r.Shims[1].Path != "" ||
		strings.Join(r.Shims[1].Checked, ":") != login+":"+missing {
		t.Fatalf("shims: %+v", r.Shims)
	}
	c := report(ModeCheck)
	if len(c.Include) != 1 || c.Include[0].Exists || c.LoginDirs != 1 || c.Effective != login+":"+missing {
		t.Fatalf("check: %+v", c)
	}
	if err := os.WriteFile(cfg, []byte("shims:\n  - nvim\n  - a/b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if e := report(ModeShims); len(e.Errors) != 1 || !strings.HasPrefix(e.Errors[0], cfg+":3: ") || len(e.Shims) != 0 {
		t.Fatalf("errors: %+v", e)
	}
}
