package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/agent"
	"github.com/flatpark/flatpark-spawn/internal/hostcmd"
	"github.com/flatpark/flatpark-spawn/internal/names"
	"github.com/flatpark/flatpark-spawn/internal/proto"
)

// Call is one run of the agent on the host.
type Call struct {
	Mode  string   // agent.Mode*
	Args  []string // after the mode
	Env   map[string]string
	Stdio [3]int // our fds that become the host's 0, 1, 2
	// Control, when set, is passed as the host's fd 3 as is (socket mode:
	// the preload library's end). Otherwise a pair is made and Reply reads it.
	Control int
	// Extra fds for the host (agent.CttyFD).
	Extra map[uint32]int
	// Ctty marks an intentional Extra[agent.CttyFD] mapping in the agent argv;
	// the explicit marker avoids guessing from an unrelated inherited fd 5.
	Ctty bool
}

// Running is a started agent.
type Running struct {
	Client *hostcmd.Client
	PID    uint32
	ctl    *os.File
	sigs   chan os.Signal
}

// notifyAsync registers the relayed signals in the background: it costs a
// few hundred microseconds of system calls, which then overlap with the
// wait for the HostCommand reply instead of adding to it.
func notifyAsync() chan os.Signal {
	ch := make(chan os.Signal, 16)
	go signal.Notify(ch, relayed...)
	return ch
}

// Start sends this executable to the host through HostCommand and runs it
// there as the agent. Signals reach the host process from here on.
func Start(c Call) (*Running, error) {
	sigs := notifyAsync()
	client, err := hostcmd.Connect()
	if err != nil {
		return nil, err
	}
	self, err := os.Open("/proc/self/exe")
	if err != nil {
		client.Close()
		return nil, err
	}
	defer self.Close()

	var ours *os.File
	hostCtl := c.Control
	if c.Control == 0 {
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			client.Close()
			return nil, err
		}
		ours = os.NewFile(uintptr(pair[0]), "control")
		hostCtl = pair[1]
		defer unix.Close(pair[1])
		// The agent reads the caller's environment first (proto.EncodeEnv).
		if err := proto.Send(pair[0], "the environment", proto.EncodeEnv(c.Env)); err != nil {
			ours.Close()
			client.Close()
			return nil, err
		}
	}
	agentArgs := []string{c.Mode}
	if c.Ctty {
		agentArgs = append(agentArgs, agent.CttyArg)
	}
	agentArgs = append(agentArgs, c.Args...)
	argv := append([]string{fmt.Sprintf("/proc/self/fd/%d", agent.SelfFD), names.ModeAgent}, agentArgs...)
	fds := map[uint32]int{
		0:               c.Stdio[0],
		1:               c.Stdio[1],
		2:               c.Stdio[2],
		agent.ControlFD: hostCtl,
		agent.SelfFD:    int(self.Fd()),
	}
	for k, v := range c.Extra {
		fds[k] = v
	}
	// The agent chdirs itself and reports a directory the host lacks;
	// HostCommand would only fail with an opaque error.
	pid, err := client.Start("/", argv, fds, nil, hostcmd.FlagWatchBus)
	if err != nil {
		if ours != nil {
			ours.Close()
		}
		client.Close()
		return nil, err
	}
	return &Running{Client: client, PID: pid, ctl: ours, sigs: sigs}, nil
}

// StartDirect runs a known host executable without the agent: the hot path
// for a name the cache says the host has (Cache.Lookup). argv[0] is the
// executable's full path. Any error (the file is gone, the directory does
// not exist on the host) means: fall back to Start.
func StartDirect(hit Hit, cwd string, args []string, env map[string]string, stdio [3]int) (*Running, error) {
	sigs := notifyAsync()
	client, err := hostcmd.Connect()
	if err != nil {
		return nil, err
	}
	e := DirectEnv(env, hit)
	argv := append([]string{hit.Path}, args...)
	pid, err := client.Start(cwd, argv, map[uint32]int{0: stdio[0], 1: stdio[1], 2: stdio[2]}, e, hostcmd.FlagWatchBus)
	if err != nil {
		client.Close()
		return nil, err
	}
	return &Running{Client: client, PID: pid, sigs: sigs}, nil
}

// DirectEnv is what a direct start passes to HostCommand: the variables of
// env whose value the host session does not already have (hit.Session),
// the login PATH, and SHELL when the session has none. The host process
// ends up with the same environment as through the agent; most of a
// sandbox's environment is inherited from the same session, and sending it
// all again costs most of a millisecond.
func DirectEnv(env map[string]string, hit Hit) map[string]string {
	e := make(map[string]string)
	for k, v := range env {
		if h, ok := hit.Session[k]; !ok || h != proto.HashValue(v) {
			e[k] = v
		}
	}
	e["PATH"] = hit.LoginPath
	if _, ok := hit.Session["SHELL"]; !ok && hit.Shell != "" && env["SHELL"] == "" {
		e["SHELL"] = hit.Shell
	}
	return e
}

// Reply reads the agent's answer.
func (r *Running) Reply() (proto.Reply, error) {
	if r.ctl == nil {
		return proto.Reply{}, errors.New("no control socket")
	}
	b, err := proto.Recv(int(r.ctl.Fd()))
	if errors.Is(err, io.EOF) {
		return proto.Reply{}, errors.New("agent exited without an answer")
	}
	if err != nil {
		return proto.Reply{}, err
	}
	return proto.Decode(b)
}

// Done waits for the agent to exit (it does right after an answer that is
// not Found) and closes. Closing the connection while the bus still has the
// exit signal to deliver makes xdg-dbus-proxy print a GLib warning on the
// app's stderr.
func (r *Running) Done() {
	select {
	case <-r.exited():
	case <-time.After(time.Second):
	}
	r.Close()
}

func (r *Running) exited() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range r.Client.Exits() {
			if _, ok := hostcmd.Status(ev, r.PID); ok {
				return
			}
		}
	}()
	return done
}

// Close releases the bus connection.
func (r *Running) Close() {
	if r.ctl != nil {
		r.ctl.Close()
	}
	r.Client.Close()
}

// Relayed signals: the ones a caller sends a child to stop, interrupt,
// reconfigure or resize it. Asking for every signal would cost about a
// millisecond per forwarded command; SIGKILL and SIGSTOP cannot be caught
// anyway (SIGKILL still ends the host process: HostCommand watches our bus
// connection).
var relayed = []os.Signal{
	unix.SIGHUP, unix.SIGINT, unix.SIGQUIT, unix.SIGTERM, unix.SIGUSR1, unix.SIGUSR2,
	unix.SIGALRM, unix.SIGWINCH, unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU, unix.SIGCONT,
}

// Wait relays signals to the host process until it exits. term, when set,
// is a PTY of ours: SIGWINCH resizes it instead of being relayed.
//
// Job control: the host process is the leader of its own session, so its
// process group is orphaned and the kernel discards SIGTSTP/SIGTTIN/SIGTTOU
// sent to it (measured: it keeps running). A stop is therefore relayed as
// SIGSTOP, after which this process stops too, as the caller's shell
// expects; SIGCONT resumes both. Our terminal gets its own settings back
// while we are stopped.
func (r *Running) Wait(term TerminalHooks) syscall.WaitStatus {
	sigs := r.sigs
	if sigs == nil {
		sigs = make(chan os.Signal, 16)
		signal.Notify(sigs, relayed...)
	}
	defer signal.Stop(sigs)
	for {
		select {
		case s := <-sigs:
			sig, ok := s.(syscall.Signal)
			if !ok {
				continue
			}
			switch sig {
			case unix.SIGWINCH:
				if term != nil {
					term.Resize()
					continue
				}
				r.signal(sig)
			case unix.SIGTSTP, unix.SIGTTIN, unix.SIGTTOU:
				r.signal(unix.SIGSTOP)
				if term != nil {
					term.Suspend()
				}
				_ = unix.Kill(os.Getpid(), unix.SIGSTOP)
			case unix.SIGCONT:
				if term != nil {
					term.Resume()
				}
				r.signal(sig)
			default:
				r.signal(sig)
			}
		case ev, ok := <-r.Client.Exits():
			if !ok {
				return syscall.WaitStatus(125 << 8)
			}
			if ws, ok := hostcmd.Status(ev, r.PID); ok {
				// From here on we only restore the terminal (up to the
				// 500 ms drain) and exit as the host process did. A signal
				// that killed us meanwhile would leave the caller's
				// terminal raw, so the fatal ones go to a channel nobody
				// reads. Not signal.Ignore: after it, ExitWith's
				// signal.Reset and re-raise no longer kill (measured: the
				// caller saw exit 143 instead of death by SIGTERM).
				signal.Notify(make(chan os.Signal, 1), unix.SIGHUP, unix.SIGINT,
					unix.SIGQUIT, unix.SIGTERM, unix.SIGUSR1, unix.SIGUSR2, unix.SIGALRM)
				return ws
			}
		}
	}
}

func (r *Running) signal(sig syscall.Signal) {
	if r.Client.Signal(r.PID, sig, true) != nil {
		_ = r.Client.Signal(r.PID, sig, false)
	}
}

// ExitWith ends this process the way the host process ended: the same exit
// code, or death by the same signal, so that the caller cannot tell it
// apart from a local child.
func ExitWith(ws syscall.WaitStatus) {
	if ws.Signaled() {
		sig := ws.Signal()
		signal.Reset(sig)
		_ = unix.Kill(os.Getpid(), sig)
		// Only reached when the signal does not kill (ignored by default).
		time.Sleep(10 * time.Millisecond)
		os.Exit(128 + int(sig))
	}
	os.Exit(ws.ExitStatus())
}
