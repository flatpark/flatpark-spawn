package pty

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Job control as a terminal sees it: a session leader whose controlling
// terminal is a fresh PTY is in the foreground; a process it starts in a
// new process group of the same session is a background job.
func TestMain(m *testing.M) {
	switch os.Getenv("FPS_TEST_FG") {
	case "leader":
		os.Stdout.WriteString(report())
		// A background job of the same terminal.
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), "FPS_TEST_FG=member")
		cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, err := cmd.Output()
		if err != nil {
			os.Exit(3)
		}
		os.Stdout.Write(out)
		os.Exit(0)
	case "member":
		os.Stdout.WriteString(" " + report())
		os.Exit(0)
	case "raw":
		t, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			os.Exit(4)
		}
		if os.Getenv("FPS_TEST_COOKED") != "1" {
			makeRaw(t)
		}
		if err := unix.IoctlSetTermios(0, unix.TCSETS, t); err != nil {
			os.Exit(5)
		}
		before, err := unix.FcntlInt(0, unix.F_GETFL, 0)
		if err != nil {
			os.Exit(6)
		}
		if os.Getenv("FPS_TEST_READER") == "1" {
			// As the relay does: another thread blocked reading the same
			// terminal (n_tty holds its read mutex while it waits).
			started := make(chan struct{})
			go func() {
				close(started)
				buf := make([]byte, 64)
				_, _ = unix.Read(0, buf)
			}()
			<-started
			time.Sleep(200 * time.Millisecond)
		}
		fmt.Fprintln(os.Stdout, "ready")
		var start [1]byte
		if _, err := io.ReadFull(os.NewFile(3, "start"), start[:]); err != nil {
			os.Exit(7)
		}
		fg := Foreground(0)
		after, err := unix.FcntlInt(0, unix.F_GETFL, 0)
		if err != nil {
			os.Exit(8)
		}
		if os.Getenv("FPS_TEST_INPUT") == "none" {
			fmt.Fprintf(os.Stdout, "foreground=%t flags-unchanged=%t\n", fg, before == after)
			os.Exit(0)
		}
		var input [1]byte
		if _, err := io.ReadFull(os.Stdin, input[:]); err != nil {
			os.Exit(9)
		}
		fmt.Fprintf(os.Stdout, "foreground=%t input=%q flags-unchanged=%t\n", fg, input, before == after)
		os.Exit(0)
	}
	// Under -race every process sleeps atexit_sleep_ms (1 s by default) in
	// os.Exit; the children here would then hit the hang timeouts below.
	if os.Getenv("GORACE") == "" {
		os.Setenv("GORACE", "atexit_sleep_ms=0")
	}
	os.Exit(m.Run())
}

func report() string {
	if Foreground(0) {
		return "fg"
	}
	return "bg"
}

func TestForeground(t *testing.T) {
	p, err := Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FPS_TEST_FG=leader")
	cmd.Stdin = p.Slave()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child: %v %q", err, out)
	}
	if ctx.Err() != nil {
		t.Fatal("foreground check timed out")
	}
	if string(out) != "fg bg" {
		t.Fatalf("got %q, want \"fg bg\"", out)
	}
}

// A hang of the first implementation, which probed with a zero-length read
// on the caller's own (blocking) fd: restoring the terminal after a
// forwarded command (and raw mode again after SIGCONT) waited for the next
// key press. A blocking
// read on an n_tty terminal holds the line discipline's read mutex while it
// waits for input, and the relay goroutine is always blocked in such a
// read; the old probe, a zero-length blocking read on the same terminal,
// queued behind it, in raw and cooked mode alike (measured on Linux 7.3: the
// cases with another reader fail with the old probe, the others pass). The
// probe must not block, must not take a typed byte, and must not change the
// caller's file status flags.
func TestForegroundDoesNotBlockOrConsumeInput(t *testing.T) {
	t.Run("no input pending", func(t *testing.T) {
		got := rawForeground(t, false)
		if want := "foreground=true flags-unchanged=true\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("another thread blocked reading the terminal", func(t *testing.T) {
		got := rawForeground(t, false, "FPS_TEST_READER=1")
		if want := "foreground=true flags-unchanged=true\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("cooked, another thread blocked reading the terminal", func(t *testing.T) {
		got := rawForeground(t, false, "FPS_TEST_READER=1", "FPS_TEST_COOKED=1")
		if want := "foreground=true flags-unchanged=true\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("input pending", func(t *testing.T) {
		got := rawForeground(t, true)
		if want := "foreground=true input=\"x\" flags-unchanged=true\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// rawForeground runs a session leader whose controlling terminal is a fresh
// PTY in raw mode (cooked with FPS_TEST_COOKED=1, a concurrent blocked
// reader with FPS_TEST_READER=1), optionally with a byte typed, and returns
// what it reports after calling Foreground. Fails if that takes two seconds.
func rawForeground(t *testing.T, typed bool, env ...string) string {
	t.Helper()
	p, err := Open()
	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}
	defer p.Close()

	startR, startW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer startR.Close()
	defer startW.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(append(os.Environ(), "FPS_TEST_FG=raw"), env...)
	if !typed {
		cmd.Env = append(cmd.Env, "FPS_TEST_INPUT=none")
	}
	cmd.Stdin, cmd.Stderr = p.Slave(), os.Stderr
	cmd.ExtraFiles = []*os.File{startR}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The child has its own duplicate now; retaining this slave would keep the
	// master readable after the child exits and hide relay shutdown bugs.
	_ = p.slave.Close()
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("child did not become ready: %q, %v", line, err)
	}
	if typed {
		if _, err := p.master.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := startW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	rest, readErr := io.ReadAll(reader)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatal("Foreground blocked for two seconds on a foreground terminal")
	}
	if readErr != nil || waitErr != nil {
		t.Fatalf("child: read=%v wait=%v output=%q", readErr, waitErr, rest)
	}
	return string(rest)
}
