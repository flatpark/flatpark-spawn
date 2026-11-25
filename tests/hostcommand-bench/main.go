// hostcommand-bench times HostCommand forwarding strategies (run on the host or in a sandbox with --talk-name=org.freedesktop.Flatpak).
// Usage: go run ./tests/hostcommand-bench <direct|directenv|envpath|loginshell|selfexe> N [PATH] cmd args...
package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName = "org.freedesktop.Flatpak"
	object  = "/org/freedesktop/Flatpak/Development"
	iface   = "org.freedesktop.Flatpak.Development"
)

func nul(s string) []byte { return append([]byte(s), 0) }

type runner struct {
	conn   *dbus.Conn
	events chan *dbus.Signal
}

func (r *runner) run(argv []string, env map[string]string, exe *os.File) (int, error) {
	a := make([][]byte, 0, len(argv)+1)
	fds := map[uint32]dbus.UnixFD{0: 0, 1: 1, 2: 2}
	if exe != nil {
		fds[4] = dbus.UnixFD(exe.Fd())
		a = append(a, nul("/proc/self/fd/4"))
	}
	for _, s := range argv {
		a = append(a, nul(s))
	}
	var pid uint32
	obj := r.conn.Object(busName, object)
	if err := obj.Call(iface+".HostCommand", 0, nul(os.Getenv("HOME")), a, fds, env, uint32(0)).Store(&pid); err != nil {
		return -1, err
	}
	for ev := range r.events {
		if ev.Name == iface+".HostCommandExited" && ev.Body[0].(uint32) == pid {
			return int(ev.Body[1].(uint32)), nil
		}
	}
	return -1, fmt.Errorf("bus closed")
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--host-noop" {
		os.Exit(0)
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		panic(err)
	}
	_ = conn.AddMatchSignal(dbus.WithMatchInterface(iface), dbus.WithMatchMember("HostCommandExited"))
	r := &runner{conn: conn, events: make(chan *dbus.Signal, 16)}
	conn.Signal(r.events)

	mode := os.Args[1]
	n, _ := strconv.Atoi(os.Args[2])
	rest := os.Args[3:]
	var times []time.Duration
	for i := 0; i < n; i++ {
		t := time.Now()
		var st int
		var err error
		switch mode {
		case "direct": // argv as-is, session-helper searches its own PATH
			st, err = r.run(rest, nil, nil)
		case "directenv": // bare argv, PATH passed in envs
			st, err = r.run(rest[1:], map[string]string{"PATH": rest[0]}, nil)
		case "envpath": // /usr/bin/env with PATH passed in envs
			st, err = r.run(append([]string{"/usr/bin/env"}, rest[1:]...), map[string]string{"PATH": rest[0]}, nil)
		case "loginshell":
			st, err = r.run(append([]string{"/bin/sh", "-c", `PATH=$("$SHELL" -lic 'printf %s "$PATH"' 2>/dev/null) exec /usr/bin/env "$@"`, "x"}, rest...), nil, nil)
		case "selfexe":
			self, _ := os.Open("/proc/self/exe")
			st, err = r.run([]string{"--host-noop"}, nil, self)
			self.Close()
		}
		times = append(times, time.Since(t))
		if i == 0 {
			fmt.Printf("%s: status=%d err=%v\n", mode, st, err)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	fmt.Printf("%s: n=%d median=%v min=%v\n", mode, n, times[len(times)/2], times[0])
}
