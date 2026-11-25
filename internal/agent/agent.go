package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/names"
	"github.com/flatpark/flatpark-spawn/internal/proto"
)

// Host fds set up by the sandbox side through HostCommand's fd map.
const (
	ControlFD = 3 // SOCK_SEQPACKET, one proto.Reply (or SCM_RIGHTS for sockets)
	SelfFD    = 4 // this executable, which HostCommand runs as /proc/self/fd/4
	// CttyFD, when present, is a PTY slave to become the command's
	// controlling terminal: the sandbox passes it when the caller is on a
	// terminal but stdin is not (`ls | fzf`), where the session helper
	// does not set one up.
	CttyFD  = 5
	CttyArg = "--ctty" // internal argv marker: CttyFD was intentionally mapped
)

// Agent modes (the word after names.ModeAgent).
const (
	ModeExec   = "exec"   // CWD TARGET ARGV0 ARGS...
	ModeLookup = "lookup" // CWD NAME
	ModeLogin  = "login"  // CWD: the user's login shell
	ModeSocket = "socket" // DOMAIN TYPE PROTOCOL COUNT
)

// CacheDir is the host-side cache directory, shared by every app.
func CacheDir(env func(string) string) string {
	dir := env("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, names.Name)
}

// send delivers the reply. A reply that cannot be delivered (too large
// even without the session fingerprint) becomes a Fail reply, and the
// caller must not exec: the sandbox would not know the command started.
func send(r proto.Reply) error {
	b, err := r.Fit()
	if err == nil {
		err = proto.Send(ControlFD, "the agent's reply", b)
	}
	if err != nil {
		_ = proto.Send(ControlFD, "an error reply", proto.Reply{Kind: proto.Fail, Msg: err.Error()}.Encode())
	}
	return err
}

func fail(format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	_ = send(proto.Reply{Kind: proto.Fail, Msg: msg})
	fmt.Fprintf(os.Stderr, "%s: %s\n", names.Name, msg)
	return 125
}

// Run is the agent's entry point; args follow names.ModeAgent.
func Run(args []string) int {
	if len(args) == 0 {
		return fail("agent: no mode")
	}
	mode, args := args[0], args[1:]
	ctty := false
	if len(args) > 0 && args[0] == CttyArg {
		ctty, args = true, args[1:]
	}
	if mode == ModeSocket {
		return socketMode(args)
	}
	overlay, err := readOverlay()
	if err != nil {
		return fail("agent: %v", err)
	}
	session := proto.EnvHashes(os.Environ())
	if len(args) == 0 {
		return fail("agent: %s needs a working directory", mode)
	}
	cwd := args[0]
	lp := NewLoginPath(os.Getenv, CacheDir(os.Getenv), cwd)
	switch mode {
	case ModeLookup:
		if len(args) != 2 {
			return fail("agent: lookup needs cwd and a name")
		}
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			_ = send(proto.Reply{Kind: proto.NoCwd, Path: cwd})
			return 0
		}
		full, path, ok := lp.Resolve(args[1])
		if !ok {
			_ = send(proto.Reply{Kind: proto.None, Gen: Generation(path), Session: session})
			return 0
		}
		if send(proto.Reply{Kind: proto.Found, Gen: Generation(path), Path: full, LoginPath: path, Shell: lp.Shell, Session: session}) != nil {
			return 125
		}
		return 0
	case ModeExec:
		if len(args) < 3 {
			return fail("agent: exec needs cwd, target and argv0")
		}
		return execMode(lp, overlay, session, args[0], args[1], append([]string{args[2]}, args[3:]...), ctty)
	case ModeLogin:
		if len(args) != 1 {
			return fail("agent: login needs cwd")
		}
		return execMode(lp, overlay, session, args[0], lp.Shell, []string{lp.Shell, "-l"}, ctty)
	}
	return fail("agent: unknown mode %q", mode)
}

// readOverlay reads the first control datagram: the caller's environment
// (proto.EncodeEnv). An empty environment is an empty datagram.
func readOverlay() (map[string]string, error) {
	b, err := proto.Recv(ControlFD)
	if errors.Is(err, io.EOF) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the caller's environment: %w", err)
	}
	return proto.DecodeEnv(b), nil
}

func execMode(lp *LoginPath, overlay map[string]string, session, cwd, target string, argv []string, ctty bool) int {
	if err := os.Chdir(cwd); err != nil {
		_ = send(proto.Reply{Kind: proto.NoCwd, Path: cwd})
		return 127
	}
	var full, path string
	var ok bool
	if strings.ContainsRune(target, '/') {
		path = lp.Get()
		full, ok = target, IsExecutable(target)
	} else {
		full, path, ok = lp.Resolve(target)
	}
	gen := Generation(path)
	if !ok {
		_ = send(proto.Reply{Kind: proto.None, Gen: gen, Session: session})
		return 127
	}
	if ctty {
		if err := takeTerminal(); err != nil {
			return fail("agent: setting the controlling terminal: %v", err)
		}
	}
	if err := send(proto.Reply{Kind: proto.Found, Gen: gen, Path: full, LoginPath: path, Shell: lp.Shell, Session: session}); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s: %v\n", names.Name, target, err)
		return 125
	}
	unix.CloseOnExec(ControlFD)
	unix.CloseOnExec(SelfFD)
	env := ExecEnv(os.Environ(), overlay, path, lp.Shell)
	err := syscall.Exec(full, argv, env)
	if errors.Is(err, syscall.ENOEXEC) {
		// No #! line: run it with sh, as execvp and shells do.
		err = syscall.Exec("/bin/sh", append([]string{"sh", full}, argv[1:]...), env)
	}
	fmt.Fprintf(os.Stderr, "%s: %s: %v\n", names.Name, target, err)
	if errors.Is(err, syscall.ENOENT) {
		return 127
	}
	return 126
}

// takeTerminal makes CttyFD our controlling terminal: a new session if we
// do not lead one yet (setsid fails harmlessly when we do), then TIOCSCTTY.
// Its process group becomes the terminal's foreground group.
func takeTerminal() error {
	// Only called when the sandbox passed CttyArg, i.e. mapped CttyFD: a
	// missing or non-terminal fd is an error, not a reason to run without.
	if _, err := unix.IoctlGetTermios(CttyFD, unix.TCGETS); err != nil {
		return err
	}
	if _, err := unix.Setsid(); err != nil && err != unix.EPERM {
		return err
	}
	var ioctlErr error
	runtime.LockOSThread()
	var set, old unix.Sigset_t
	set.Val[(unix.SIGTTOU-1)/64] |= 1 << ((uint(unix.SIGTTOU) - 1) % 64)
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &set, &old); err != nil {
		runtime.UnlockOSThread()
		return err
	}
	ioctlErr = unix.IoctlSetInt(CttyFD, unix.TIOCSCTTY, 0)
	_ = unix.PthreadSigmask(unix.SIG_SETMASK, &old, nil)
	runtime.UnlockOSThread()
	if ioctlErr != nil {
		return ioctlErr
	}
	return unix.Close(CttyFD)
}

// ExecEnv is the host environment for the command: the session helper's
// environment, with the caller's (overlay) on top, the login PATH, and
// SHELL when neither has one.
func ExecEnv(environ []string, overlay map[string]string, path, shell string) []string {
	out := make([]string, 0, len(environ)+len(overlay)+2)
	hasShell := overlay["SHELL"] != ""
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" {
			continue
		}
		if _, over := overlay[k]; over {
			continue
		}
		if k == "SHELL" {
			hasShell = true
		}
		out = append(out, kv)
	}
	for k, v := range overlay {
		if k != "PATH" {
			out = append(out, k+"="+v)
		}
	}
	out = append(out, "PATH="+path)
	if !hasShell && shell != "" {
		out = append(out, "SHELL="+shell)
	}
	return out
}

func socketMode(args []string) int {
	if len(args) != 4 {
		return 2
	}
	var v [4]int
	for i, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil {
			return 2
		}
		v[i] = n
	}
	var fds []int
	switch v[3] {
	case 1:
		fd, err := unix.Socket(v[0], v[1], v[2])
		if err != nil {
			return 1
		}
		fds = []int{fd}
	case 2:
		pair, err := unix.Socketpair(v[0], v[1], v[2])
		if err != nil {
			return 1
		}
		fds = pair[:]
	default:
		return 2
	}
	if err := unix.Sendmsg(ControlFD, []byte{'s'}, unix.UnixRights(fds...), nil, 0); err != nil {
		return 1
	}
	return 0
}
