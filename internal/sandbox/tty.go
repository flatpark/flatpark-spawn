package sandbox

import (
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/pty"
)

// Terminal is the forwarding path's answer to a caller on a terminal.
//
// Handing the caller's terminal to HostCommand as is does not work: it is
// already the controlling terminal of the caller's session (the app's
// shell), so the session helper cannot make it the host process's, and the
// host process ends up without one: opening /dev/tty fails with ENXIO
// (sudo and ssh prompts, git's editor, fzf, less, vim, zsh's line editor).
// So, as the CLI does, a pseudo-terminal of our own goes in between:
//
//   - the fds among 0, 1, 2 that are terminals become its slave; the others
//     (pipes, files) pass through as they are (`ls | fzf`, `prog | less`);
//   - the slave becomes the host process's controlling terminal: the
//     session helper makes it so when it is stdin; otherwise the agent does
//     (agent.CttyFD), which is why such a call skips the direct start;
//   - our terminal is put in raw mode and bytes are copied both ways, so
//     Ctrl-C reaches the host process through its terminal; the window size
//     follows ours.
type Terminal struct {
	pty     *pty.PTY
	in, out *os.File
	begun   bool
	Stdio   [3]int // our fds that become the host's 0, 1, 2
	// Ctty is the slave to pass as agent.CttyFD, or -1 when stdin is the
	// slave (the session helper sets it up then).
	Ctty   int
	opened []*os.File
}

// SetupTerminal returns nil when none of 0, 1, 2 is a terminal, when we
// are not in its foreground process group, or when no PTY can be had: the
// command then runs with the caller's fds, as before.
func SetupTerminal() *Terminal {
	var tty [3]bool
	first := -1
	for fd := 0; fd < 3; fd++ {
		if pty.IsTerminal(fd) {
			tty[fd] = true
			if first < 0 {
				first = fd
			}
		}
	}
	if first < 0 || !pty.Foreground(first) {
		return nil
	}
	t := &Terminal{Ctty: -1}
	var devtty *os.File
	if !tty[0] || (!tty[1] && !tty[2]) {
		var err error
		devtty, err = os.OpenFile("/dev/tty", os.O_RDWR|unix.O_CLOEXEC, 0)
		if err != nil {
			// Without a distinct controlling-terminal fd we cannot preserve
			// pipe semantics safely. Leave the call unchanged.
			return nil
		}
		t.opened = append(t.opened, devtty)
	}
	in := os.Stdin
	if !tty[0] {
		in = devtty
	}
	out := os.Stdout
	switch {
	case tty[1]:
	case tty[2]:
		out = os.Stderr
	default:
		out = devtty
	}
	p, err := pty.Open()
	if err != nil {
		t.close()
		return nil
	}
	t.pty = p
	slave := int(p.Slave().Fd())
	for fd := 0; fd < 3; fd++ {
		t.Stdio[fd] = fd
		if tty[fd] {
			t.Stdio[fd] = slave
		}
	}
	if !tty[0] {
		t.Ctty = slave
	}
	t.in, t.out = in, out
	p.SizeFrom(out, in)
	return t
}

// Begin starts relaying once the command runs: raw mode and copying wait
// until then, so that nothing typed is swallowed when the host turns out
// not to have the command and a local fallback reads it instead.
func (t *Terminal) Begin() {
	if t != nil && !t.begun {
		t.begun = true
		t.pty.Start(t.in, t.out)
	}
}

// HostHasSlave drops our copy of the slave once HostCommand holds its own.
func (t *Terminal) HostHasSlave() {
	if t != nil {
		t.pty.HostHasSlave()
	}
}

// Finish drains the last output and restores our terminal. Call it before
// exiting, before a local fallback and before printing an error.
func (t *Terminal) Finish() {
	if t == nil {
		return
	}
	if t.begun {
		t.pty.Finish(500 * time.Millisecond)
	} else {
		t.pty.Close()
	}
	t.close()
}

func (t *Terminal) close() {
	for _, f := range t.opened {
		f.Close()
	}
	t.opened = nil
}

// Hooks is what Running.Wait needs from it (nil when there is none).
func (t *Terminal) Hooks() TerminalHooks {
	if t == nil {
		return nil
	}
	return t.pty
}

// TerminalHooks: a PTY of ours between the caller's terminal and the host.
type TerminalHooks interface {
	Resize()
	Suspend()
	Resume()
}

var _ TerminalHooks = (*pty.PTY)(nil)
