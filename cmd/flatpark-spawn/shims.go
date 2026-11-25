package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/agent"
	"github.com/flatpark/flatpark-spawn/internal/config"
	"github.com/flatpark/flatpark-spawn/internal/names"
	"github.com/flatpark/flatpark-spawn/internal/proto"
	"github.com/flatpark/flatpark-spawn/internal/sandbox"
)

// shimsTimeout bounds making the shims at app start: past it the app
// starts with the shims it had.
const shimsTimeout = 2 * time.Second

// checkTimeout bounds `check` (it may run the login shell).
const checkTimeout = 30 * time.Second

// atExecFn is AT_EXECFN: the path the kernel was asked to execute.
const atExecFn = 31

// execFn is the path this program was executed by, as the kernel got it
// (AT_EXECFN). argv[0] does not tell: execvp("nvim") finds the shim in
// PATH and passes argv[0] "nvim". The string lives on our stack; it is
// read through /proc/self/mem rather than an unsafe pointer.
func execFn() string {
	auxv, err := unix.Auxv()
	if err != nil {
		return ""
	}
	var addr uintptr
	for _, kv := range auxv {
		if kv[0] == atExecFn {
			addr = kv[1]
		}
	}
	if addr == 0 {
		return ""
	}
	mem, err := os.Open("/proc/self/mem")
	if err != nil {
		return ""
	}
	defer mem.Close()
	var out []byte
	buf := make([]byte, 256)
	for len(out) < 4096 {
		n, err := mem.ReadAt(buf, int64(addr)+int64(len(out)))
		if i := strings.IndexByte(string(buf[:n]), 0); i >= 0 {
			return string(append(out, buf[:i]...))
		}
		out = append(out, buf[:n]...)
		if err != nil || n == 0 {
			break
		}
	}
	return ""
}

// shimName: this run is a shim when the file executed lies in the app's
// shim directory. Our own name and the agent's /proc/self/fd/N never are,
// which keeps the forwarding paths from looking.
func shimName() (string, bool) {
	a0 := os.Args[0]
	if filepath.Base(a0) == names.Name || strings.HasPrefix(a0, "/proc/self/fd/") {
		return "", false
	}
	dir := sandbox.ShimDir()
	if dir == "" {
		return "", false
	}
	for _, p := range []string{execFn(), a0} {
		if name, ok := sandbox.InShimDir(p, dir); ok {
			return name, true
		}
	}
	return "", false
}

// runShim runs NAME through its shim (DESIGN.md, shim): in the sandbox when
// the name is in FLATPARK_SPAWN_LOCAL, or when the arguments or the working
// directory are sandbox-private and the sandbox has the command (the
// preload library's rule); otherwise on the host, falling back to the
// sandbox's own copy when the host has none.
func runShim(name string, args []string) int {
	argv0 := os.Args[0]
	if strings.ContainsRune(argv0, '/') {
		argv0 = name
	}
	depth := names.Env("SHIM_DEPTH")
	if os.Getenv(depth) != "" {
		errorf("shim %s: run again from its own local fallback (%s is set); stopping", name, depth)
		return 126
	}
	local := func() int {
		p, ok := sandbox.LocalPath(name)
		if !ok {
			fmt.Fprintf(os.Stderr, "%s: command not found\n", name)
			return exitNotFound
		}
		// Without the shim directory in PATH, and marked: the local
		// program cannot come back here (a wrapper script that runs its
		// own name, say).
		env := make([]string, 0, len(os.Environ())+1)
		for _, kv := range os.Environ() {
			if v, ok := strings.CutPrefix(kv, "PATH="); ok {
				kv = "PATH=" + sandbox.WithoutDir(v, sandbox.ShimDir())
			}
			env = append(env, kv)
		}
		env = append(env, depth+"=1")
		err := syscall.Exec(p, append([]string{argv0}, args...), env)
		errorf("%s: %v", p, err)
		return 126
	}
	if listed(os.Getenv(names.Env("LOCAL")), name) {
		return local()
	}
	if sandbox.ArgsPrivate(cwd(), args, sandbox.TmpShared("/.flatpak-info")) {
		if _, ok := sandbox.LocalPath(name); ok {
			return local()
		}
	}
	reply, err := hostRun(name, argv0, args)
	if err == nil && reply.Kind == proto.NoCwd {
		if _, ok := sandbox.LocalPath(name); !ok {
			errorf("working directory %s does not exist on the host", reply.Path)
			return exitNotFound
		}
	}
	return local()
}

// askReport runs the agent in mode (shims or check) for the current
// directory and returns its report, within timeout. The host side's stdin
// and stdout are /dev/null; its stderr is ours.
func askReport(mode string, timeout time.Duration) (config.Report, error) {
	type result struct {
		rep config.Report
		err error
	}
	done := make(chan result, 1)
	var running *sandbox.Running
	started := make(chan *sandbox.Running, 1)
	go func() {
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer null.Close()
		n := int(null.Fd())
		r, err := sandbox.Start(sandbox.Call{Mode: mode, Args: []string{cwd()}, Stdio: [3]int{n, n, 2}})
		if err != nil {
			done <- result{err: err}
			return
		}
		started <- r
		reply, err := r.Reply()
		r.Done()
		switch {
		case err != nil:
			done <- result{err: err}
		case reply.Kind == proto.Report:
			rep, err := config.DecodeReport(reply.Msg)
			done <- result{rep, err}
		case reply.Kind == proto.Fail:
			done <- result{err: errors.New(reply.Msg)}
		default:
			done <- result{err: fmt.Errorf("unexpected agent reply %q", reply.Kind)}
		}
	}()
	select {
	case res := <-done:
		return res.rep, res.err
	case <-time.After(timeout):
		select {
		case running = <-started:
			running.Close() // HostCommand ends the host side with the connection
		default:
		}
		return config.Report{}, fmt.Errorf("no answer from the host within %v", timeout)
	}
}

// makeShims is enable.sh's --x-shims DIR: DIR becomes the set of links for
// the configured shims the host has. The links point at the command as
// enable.sh ran it (os.Args[0], the extension's path). Any failure leaves
// DIR as it was; enable.sh goes on either way.
func makeShims(args []string) int {
	if len(args) != 1 || !filepath.IsAbs(args[0]) {
		errorf("%s needs an absolute directory", names.ModeShims)
		return 2
	}
	target := os.Args[0]
	if !filepath.IsAbs(target) {
		target = filepath.Join(names.Mount, "bin", names.Name)
	}
	rep, err := askReport(agent.ModeShims, shimsTimeout)
	if err != nil {
		errorf("shims not updated: %v", err)
		return 1
	}
	if len(rep.Errors) > 0 {
		for _, e := range rep.Errors {
			errorf("%s", e)
		}
		errorf("configuration not used, shims not updated (see %s check)", names.Name)
		return 1
	}
	for _, s := range rep.Shims {
		if s.Path == "" {
			errorf("shim %s: not found in the host path (checked: %s); not created", s.Name, strings.Join(s.Checked, ":"))
		}
	}
	if err := sandbox.WriteShims(args[0], target, rep.Found()); err != nil {
		errorf("shims not updated: %v", err)
		return 1
	}
	return 0
}

// check is `flatpark-spawn check`: in an app, through the agent on the host
// plus the app's shim directory; on the host, right here.
func check() int {
	home := os.Getenv("HOME")
	if _, err := os.Stat("/.flatpak-info"); err == nil {
		rep, err := askReport(agent.ModeCheck, checkTimeout)
		if err != nil {
			errorf("check: %v", err)
			return 1
		}
		sb := &config.Sandbox{Dir: sandbox.ShimDir()}
		sb.Names, sb.Missing = sandbox.ShimNames(sb.Dir)
		sb.Missing = !sb.Missing
		return config.Render(os.Stdout, rep, home, sb)
	}
	file := config.Path(os.Getenv)
	cfg, exists, loadErr := config.Load(file, home)
	dir := cwd()
	s := &agent.Search{LP: agent.NewLoginPath(os.Getenv, agent.CacheDir(os.Getenv), dir), Cfg: cfg}
	return config.Render(os.Stdout, s.Report(file, exists, loadErr), home, nil)
}
