// flatpark-spawn runs commands on the host from inside a Flatpak sandbox.
//
// It is three programs in one static binary:
//   - the user-facing command (and host-spawn's, by basename; see internal/cli);
//   - the sandbox end of libflatpark-spawn.so's forwarding (internal modes,
//     names.Mode*), never PTY, never interactive;
//   - the host agent, when HostCommand runs this same file on the host.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/flatpark/flatpark-spawn/internal/agent"
	"github.com/flatpark/flatpark-spawn/internal/cli"
	"github.com/flatpark/flatpark-spawn/internal/hostcmd"
	"github.com/flatpark/flatpark-spawn/internal/names"
	"github.com/flatpark/flatpark-spawn/internal/proto"
	"github.com/flatpark/flatpark-spawn/internal/pty"
	"github.com/flatpark/flatpark-spawn/internal/sandbox"
)

// exitNotFound is what a shell returns for a command it cannot find.
const exitNotFound = 127

func main() {
	// GOMAXPROCS=1 set by libflatpark-spawn.so for our own start-up only
	// (preload.c, helper_envp): not for the host, not for a local fallback.
	if os.Getenv(names.Env("GOMAXPROCS")) == "1" {
		os.Unsetenv("GOMAXPROCS")
		os.Unsetenv(names.Env("GOMAXPROCS"))
	}
	if len(os.Args) >= 2 && internal(os.Args[0]) {
		switch os.Args[1] {
		case names.ModeAgent:
			os.Exit(agent.Run(os.Args[2:]))
		case names.ModeProbe:
			os.Exit(probe())
		case names.ModeExec:
			os.Exit(forwardExec(os.Args[2:]))
		case names.ModeShell:
			os.Exit(forwardShell(os.Args[2:]))
		case names.ModeLookup:
			os.Exit(lookup(os.Args[2:]))
		case names.ModeSocket:
			os.Exit(socketHelper(os.Args[2:]))
		}
	}
	os.Exit(runCLI(os.Args))
}

// internal: the internal modes answer only to our own name and to the
// agent's /proc/self/fd/N, never to a host-spawn shim.
func internal(argv0 string) bool {
	return cli.KindOf(argv0) == cli.Ours || strings.HasPrefix(argv0, "/proc/self/fd/")
}

func errorf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, names.Name+": "+format+"\n", a...)
}

func hostEnv() map[string]string {
	return sandbox.HostEnv(os.Environ(), sandbox.TmpShared("/.flatpak-info"))
}

// cwd is the physical working directory: the same one the preload library
// keyed its decision on, and the one the login PATH is taken in.
func cwd() string { return sandbox.Getwd() }

// probe: can this app use HostCommand at all? Silent; enable.sh decides on
// the exit code.
func probe() int {
	c, err := hostcmd.Connect()
	if err != nil {
		return 1
	}
	defer c.Close()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 1
	}
	defer null.Close()
	n := int(null.Fd())
	pid, err := c.Start("/", []string{"true"}, map[uint32]int{0: n, 1: n, 2: n}, nil, 0)
	if err != nil {
		return 1
	}
	done := make(chan syscall.WaitStatus, 1)
	go func() {
		ws, _ := c.Wait(pid)
		done <- ws
	}()
	select {
	case ws := <-done:
		if ws.Exited() && ws.ExitStatus() == 0 {
			return 0
		}
	case <-time.After(5 * time.Second):
	}
	return 1
}

// localPath finds name in the sandbox's PATH.
func localPath(name string) (string, bool) {
	if strings.ContainsRune(name, '/') {
		return "", false
	}
	return agent.LookPath(name, os.Getenv("PATH"))
}

// hostRun runs target on the host for the forwarding paths (the preload
// library's exec and bash's command_not_found_handle): directly from a
// fresh cache entry, else through the agent. It does not return when the
// command ran (this process then ends as the host process did); otherwise
// it returns why it did not, with our terminal restored.
func hostRun(target, argv0 string, rest []string) (proto.Reply, error) {
	bare := !strings.ContainsRune(target, '/')
	cache := sandbox.CacheFromEnv()
	dir, env := cwd(), hostEnv()
	term := sandbox.SetupTerminal()
	stdio, extra := [3]int{0, 1, 2}, map[uint32]int(nil)
	ctty := false
	if term != nil {
		stdio = term.Stdio
		if term.Ctty >= 0 {
			extra = map[uint32]int{agent.CttyFD: term.Ctty}
			ctty = true
		}
	}
	// A direct start cannot give the host process a controlling terminal
	// other than its stdin; that takes the agent.
	if bare && (term == nil || term.Ctty < 0) {
		if hit, ok := cache.Lookup(target); ok {
			if r, err := sandbox.StartDirect(hit, dir, rest, env, stdio); err == nil {
				term.HostHasSlave()
				waitAndExit(r, term)
			}
		}
	}
	r, err := sandbox.Start(sandbox.Call{
		Mode:  agent.ModeExec,
		Args:  append([]string{dir, target, argv0}, rest...),
		Env:   env,
		Stdio: stdio,
		Extra: extra,
		Ctty:  ctty,
	})
	if err != nil {
		term.Finish()
		return proto.Reply{}, fmt.Errorf("cannot run on the host: %w", err)
	}
	term.HostHasSlave()
	reply, err := r.Reply()
	if err != nil {
		r.Done()
		term.Finish()
		return proto.Reply{}, err
	}
	if bare {
		cache.RecordReply(target, reply)
	}
	if reply.Kind == proto.Found {
		waitAndExit(r, term)
	}
	r.Done()
	term.Finish()
	return reply, nil
}

func waitAndExit(r *sandbox.Running, term *sandbox.Terminal) {
	term.Begin()
	ws := r.Wait(term.Hooks())
	r.Close()
	term.Finish()
	sandbox.ExitWith(ws)
}

// forwardExec is the preload library's exec of a host command:
// TARGET ARGV0 ARGS... When the host turns out not to have it after all (the
// cache was stale) the sandbox's own copy runs, if there is one.
func forwardExec(args []string) int {
	if len(args) < 2 {
		errorf("%s needs a target and argv0", names.ModeExec)
		return 2
	}
	target, argv0, rest := args[0], args[1], args[2:]
	local := func(why string) int {
		if p, ok := localPath(target); ok {
			err := syscall.Exec(p, append([]string{argv0}, rest...), os.Environ())
			errorf("%s: %v", target, err)
			return 126
		}
		errorf("%s: %s", target, why)
		return exitNotFound
	}
	reply, err := hostRun(target, argv0, rest)
	if err != nil {
		return local(err.Error())
	}
	switch reply.Kind {
	case proto.None:
		if !strings.ContainsRune(target, '/') {
			return local("command not found")
		}
		return local("No such file or directory")
	case proto.NoCwd:
		return local(fmt.Sprintf("working directory %s does not exist on the host", reply.Path))
	}
	return local(reply.Msg)
}

// forwardShell is bash's command_not_found_handle: SHELL CMD ARGS... The
// shell has already failed to find CMD in the sandbox.
func forwardShell(args []string) int {
	if len(args) < 2 {
		return exitNotFound
	}
	shell, name, rest := args[0], args[1], args[2:]
	if sandbox.ValidName(name) && !listed(os.Getenv(names.Env("LOCAL")), name) {
		if reply, err := hostRun(name, name, rest); err == nil && reply.Kind == proto.NoCwd {
			errorf("working directory %s does not exist on the host", reply.Path)
		}
	}
	fmt.Fprintf(os.Stderr, "%s: %s: command not found\n", shell, name)
	return exitNotFound
}

func listed(list, name string) bool {
	for _, n := range strings.Split(list, ":") {
		if n == name {
			return true
		}
	}
	return false
}

// lookup answers the preload library: 0 the host has NAME, 1 it does not,
// 2 the host could not be asked. The answer is cached either way.
func lookup(args []string) int {
	if len(args) != 1 || !sandbox.ValidName(args[0]) {
		return 2
	}
	r, err := sandbox.Start(sandbox.Call{
		Mode:  agent.ModeLookup,
		Args:  []string{cwd(), args[0]},
		Stdio: [3]int{0, 1, 2},
	})
	if err != nil {
		return 2
	}
	defer r.Done()
	reply, err := r.Reply()
	if err != nil {
		return 2
	}
	sandbox.CacheFromEnv().RecordReply(args[0], reply)
	switch reply.Kind {
	case proto.Found:
		return 0
	case proto.None:
		return 1
	}
	return 2
}

// socketHelper creates a socket on the host for the preload library, whose
// end of the control socket is our fd 3: DOMAIN TYPE PROTOCOL COUNT.
func socketHelper(args []string) int {
	if len(args) != 4 {
		return 2
	}
	r, err := sandbox.Start(sandbox.Call{
		Mode:    agent.ModeSocket,
		Args:    args,
		Stdio:   [3]int{0, 1, 2},
		Control: agent.ControlFD,
	})
	if err != nil {
		return 1
	}
	defer r.Close()
	ws, err := r.Client.Wait(r.PID)
	if err != nil || !ws.Exited() {
		return 1
	}
	return ws.ExitStatus()
}

func runCLI(argv []string) int {
	o, err := cli.Parse(argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if o.Help {
		fmt.Fprint(os.Stderr, o.Usage)
		return 0
	}
	if o.Version {
		fmt.Println(names.Version)
		return 0
	}
	dir := cwd()
	if o.Cwd != "" {
		if dir, err = filepath.Abs(o.Cwd); err != nil {
			errorf("%v", err)
			return exitNotFound
		}
	}
	mode, callArgs := agent.ModeLogin, []string{dir}
	args := o.Args
	if len(args) == 0 {
		args = o.DefaultCommand()
	}
	if len(args) > 0 {
		mode = agent.ModeExec
		callArgs = append([]string{dir, args[0], args[0]}, args[1:]...)
	}
	var env map[string]string
	if o.Kind == cli.Ours {
		env = hostEnv()
		if o.EnvSet {
			for k, v := range sandbox.CompatEnv(o.Env) {
				env[k] = v
			}
		}
	} else {
		env = sandbox.CompatEnv(o.Env)
	}

	stdio := [3]int{0, 1, 2}
	var term *pty.PTY
	// Not from a background job: it may not change or read the terminal.
	if o.WantPty(pty.IsTerminal(0), pty.IsTerminal(1)) && pty.Foreground(0) {
		if term, err = pty.Open(); err == nil {
			term.Start(os.Stdin, os.Stdout)
			s := int(term.Slave().Fd())
			stdio = [3]int{s, s, s}
		} else {
			term = nil
		}
	}
	finish := func() {
		if term != nil {
			term.Finish(500 * time.Millisecond)
		}
	}
	r, err := sandbox.Start(sandbox.Call{Mode: mode, Args: callArgs, Env: env, Stdio: stdio})
	if term != nil {
		term.HostHasSlave()
	}
	if err != nil {
		finish()
		errorf("cannot run on the host: %v", err)
		return exitNotFound
	}
	reply, err := r.Reply()
	if err != nil {
		r.Done()
		finish()
		errorf("%v", err)
		return exitNotFound
	}
	switch reply.Kind {
	case proto.Found:
		var hooks sandbox.TerminalHooks
		if term != nil {
			hooks = term
		}
		ws := r.Wait(hooks)
		r.Close()
		finish()
		sandbox.ExitWith(ws)
	case proto.None:
		finish()
		what := "login shell"
		if len(args) > 0 {
			what = args[0]
		}
		errorf("%s: command not found", what)
	case proto.NoCwd:
		finish()
		errorf("working directory %s does not exist on the host", reply.Path)
	default:
		finish()
		errorf("%s", reply.Msg)
	}
	r.Done()
	return exitNotFound
}
