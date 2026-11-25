// Package pty gives a host process a terminal of its own.
//
// Adapted from pty.go of 1player/host-spawn v1.6.2
// (https://github.com/1player/host-spawn, Copyright 2022 Stéphane
// Travostino, MIT-0): allocate a pseudo-terminal in the sandbox, hand its
// slave to HostCommand as fds 0-2, put our own terminal in raw mode and
// copy bytes both ways, and pass on window-size changes.
//
// Differences from upstream: golang.org/x/sys instead of github.com/pkg/term;
// our copy of the slave is closed once the host has its own, so that the
// master reports EOF when the host side is done and the last output is
// drained before exit (upstream stops copying at exit and can lose it).
package pty

import (
	"io"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// PTY is one pseudo-terminal pair plus the state of our own terminal.
type PTY struct {
	master, slave *os.File
	in, out       *os.File // our terminal: keystrokes from in, output to out
	saved         *unix.Termios
	output        chan struct{}
}

// IsTerminal reports whether fd is a terminal.
func IsTerminal(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}

// Open allocates a pseudo-terminal.
func Open() (*PTY, error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.IoctlSetPointerInt(mfd, unix.TIOCSPTLCK, 0); err != nil {
		unix.Close(mfd)
		return nil, err
	}
	// TIOCGPTPEER needs no /dev/pts path, which may differ in a container.
	sfd, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCGPTPEER,
		uintptr(unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC))
	if errno != 0 {
		n, err := unix.IoctlGetUint32(mfd, unix.TIOCGPTN)
		if err != nil {
			unix.Close(mfd)
			return nil, err
		}
		fd, err := unix.Open("/dev/pts/"+strconv.FormatUint(uint64(n), 10), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			unix.Close(mfd)
			return nil, err
		}
		sfd = uintptr(fd)
	}
	return &PTY{
		master: os.NewFile(uintptr(mfd), "ptmx"),
		slave:  os.NewFile(sfd, "pts"),
		output: make(chan struct{}),
	}, nil
}

// Slave is the host process's terminal.
func (p *PTY) Slave() *os.File { return p.slave }

// Start makes our terminal (in) raw, copies in to the PTY and the PTY to
// out, and gives the PTY our window size. in and out are usually stdin and
// stdout; for `printf x | prog` in is the controlling terminal (/dev/tty).
func (p *PTY) Start(in, out *os.File) {
	p.in, p.out = in, out
	foreground := p.raw()
	// From the background, reading the terminal would stop us (SIGTTIN) even
	// if the host command never reads: only the foreground relays input.
	if foreground {
		go func() { _, _ = io.Copy(p.master, in) }()
	}
	go func() {
		_, _ = io.Copy(out, p.master)
		close(p.output)
	}()
	p.Resize()
}

func (p *PTY) raw() bool {
	if !Foreground(int(p.in.Fd())) {
		return false
	}
	if t, err := unix.IoctlGetTermios(int(p.in.Fd()), unix.TCGETS); err == nil {
		saved := *t
		makeRaw(t)
		if setTermios(int(p.in.Fd()), t) == nil {
			p.saved = &saved
			return true
		}
	}
	return false
}

// restore puts back the settings saved by raw. SIGTTOU is blocked around
// tcsetattr so restoration also works if job control moved us to the
// background while the host command was exiting.
func (p *PTY) restore() {
	if p.saved != nil {
		_ = setTermios(int(p.in.Fd()), p.saved)
	}
}

// Suspend gives our terminal back its own settings (before we stop).
func (p *PTY) Suspend() { p.restore() }

// Resume makes it raw again and passes on a size changed meanwhile.
func (p *PTY) Resume() {
	p.raw()
	p.Resize()
}

// SizeFrom gives the PTY the window size of the first of files that is a
// terminal, before anything runs on it.
func (p *PTY) SizeFrom(files ...*os.File) {
	for _, f := range files {
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil {
			_ = setWinsize(int(p.master.Fd()), ws)
			return
		}
	}
}

// Close releases a PTY that was never started.
func (p *PTY) Close() {
	p.master.Close()
	p.slave.Close()
}

// HostHasSlave drops our copy of the slave once HostCommand holds its own.
func (p *PTY) HostHasSlave() { p.slave.Close() }

// Resize copies our window size to the PTY; the kernel then signals the
// host's foreground process group with SIGWINCH.
func (p *PTY) Resize() {
	for _, f := range []*os.File{p.out, p.in} {
		if f == nil {
			continue
		}
		if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil {
			_ = setWinsize(int(p.master.Fd()), ws)
			return
		}
	}
}

// Finish waits briefly for the remaining output (a background process on
// the host may hold the terminal open) and restores our terminal.
func (p *PTY) Finish(wait time.Duration) {
	select {
	case <-p.output:
	case <-time.After(wait):
	}
	// A host-side descendant may keep the slave open indefinitely. Closing
	// our master does not interrupt the output copy already blocked in
	// read(2) (the fd is blocking and not in Go's poller, so the close is
	// deferred until that read returns); it does stop any later one. The
	// caller exits right after, so at most a write already under way can
	// land after the terminal is restored.
	_ = p.master.Close()
	p.restore()
}

// Foreground reports whether we are in the foreground process group of the
// terminal fd. Only then may we change its settings or read from it: from
// the background, tcsetattr raises SIGTTOU and read SIGTTIN, and with those
// signals caught (signal.Notify) the system call restarts forever.
//
// Comparing tcgetpgrp with getpgrp does not work inside a Flatpak sandbox:
// a process group outside the sandbox's PID namespace reads as 0 on both
// sides. Instead, open /dev/tty as a separate nonblocking open file
// description and do a zero-length read with SIGTTIN blocked. The kernel's
// job-control check runs first and fails it with EIO for a background group;
// 0 or EAGAIN means foreground. It must be nonblocking: n_tty's read takes a
// per-terminal mutex (with mutex_trylock, i.e. EAGAIN, for O_NONBLOCK) that
// a blocking reader holds while it waits for input, and our own relay
// goroutine is such a reader, so a blocking probe on the caller's fd (as
// first implemented) waited for the next key press. O_NONBLOCK is confined to this new fd: the caller's fd flags
// and input queue are untouched. If /dev/tty cannot be opened we have no
// controlling terminal, job control does not apply, and nothing tells which
// terminal is which: report background, so that nothing is changed (the
// command runs with the caller's fds, as when there is no terminal).
func Foreground(fd int) bool {
	if !IsTerminal(fd) {
		return false
	}
	probe, err := unix.Open("/dev/tty", unix.O_RDONLY|unix.O_NOCTTY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer unix.Close(probe)
	var readErr error
	if err := withSignalBlocked(unix.SIGTTIN, func() { _, readErr = unix.Read(probe, nil) }); err != nil {
		return false
	}
	return readErr == nil || readErr == unix.EAGAIN || readErr == unix.EWOULDBLOCK
}

// withSignalBlocked runs fn on one locked OS thread. Terminal state-changing
// ioctls are allowed for background jobs when SIGTTOU is blocked; doing this
// locally avoids changing process-wide signal handling.
func withSignalBlocked(sig syscall.Signal, fn func()) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var set, old unix.Sigset_t
	set.Val[(sig-1)/64] |= 1 << ((uint(sig) - 1) % 64)
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &set, &old); err != nil {
		return err
	}
	defer unix.PthreadSigmask(unix.SIG_SETMASK, &old, nil)
	fn()
	return nil
}

func setTermios(fd int, t *unix.Termios) error {
	var ioctlErr error
	if err := withSignalBlocked(unix.SIGTTOU, func() {
		ioctlErr = unix.IoctlSetTermios(fd, unix.TCSETS, t)
	}); err != nil {
		return err
	}
	return ioctlErr
}

func setWinsize(fd int, ws *unix.Winsize) error {
	var ioctlErr error
	if err := withSignalBlocked(unix.SIGTTOU, func() {
		ioctlErr = unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws)
	}); err != nil {
		return err
	}
	return ioctlErr
}

// makeRaw is cfmakeraw(3).
func makeRaw(t *unix.Termios) {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
}
