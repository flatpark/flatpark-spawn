// Package hostcmd talks to org.freedesktop.Flatpak.Development, the
// session-helper interface that `flatpak-spawn --host` and host-spawn use.
// The app needs --talk-name=org.freedesktop.Flatpak for any of it.
//
// Every forwarded command opens a fresh bus connection, so two things are
// trimmed from godbus's default setup, both through its public API: only
// EXTERNAL authentication is offered (godbus would otherwise look up the
// user and try other mechanisms), and AddMatch is sent without waiting for
// its reply. The bus handles one connection's messages in order, so the
// match is in place before the HostCommand that follows.
//
// A fully pipelined connection (SASL, Hello, AddMatch and HostCommand in one
// write, as sd-bus does) was measured and dropped: it saved 0.26-0.54 ms per
// forward but needed to write godbus's unexported Message.serial through
// reflect/unsafe. See DESIGN.md, 性能.
package hostcmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/godbus/dbus/v5"
)

const (
	busName = "org.freedesktop.Flatpak"
	object  = "/org/freedesktop/Flatpak/Development"
	iface   = "org.freedesktop.Flatpak.Development"

	// FlagWatchBus makes the session helper kill the host process when
	// this bus connection goes away, as the process would die with us if
	// it were our own child.
	FlagWatchBus uint32 = 2
)

// Client is one session bus connection, subscribed to HostCommandExited
// before anything is started so that no exit can be missed.
type Client struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	signals chan *dbus.Signal
}

// Connect opens the session bus.
func Connect() (*Client, error) {
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, err
	}
	if err := conn.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, err
	}
	c := &Client{
		conn:    conn,
		obj:     conn.Object(busName, dbus.ObjectPath(object)),
		signals: make(chan *dbus.Signal, 32),
	}
	conn.Signal(c.signals)
	conn.BusObject().Go("org.freedesktop.DBus.AddMatch", dbus.FlagNoReplyExpected, nil, MatchRule)
	return c, nil
}

// MatchRule selects the exit signals.
var MatchRule = fmt.Sprintf("type='signal',interface='%s',member='HostCommandExited'", iface)

// Close closes the connection.
func (c *Client) Close() { c.conn.Close() }

// Start runs argv on the host. fds maps the host process's fd numbers to
// ours; env is added to the session helper's environment.
func (c *Client) Start(cwd string, argv []string, fds map[uint32]int, env map[string]string, flags uint32) (uint32, error) {
	if len(argv) == 0 {
		return 0, errors.New("empty command")
	}
	av := make([][]byte, len(argv))
	for i, a := range argv {
		av[i] = append([]byte(a), 0)
	}
	dfds := make(map[uint32]dbus.UnixFD, len(fds))
	for k, v := range fds {
		dfds[k] = dbus.UnixFD(v)
	}
	if env == nil {
		env = map[string]string{}
	}
	var pid uint32
	err := c.obj.Call(iface+".HostCommand", 0, append([]byte(cwd), 0), av, dfds, env, flags).Store(&pid)
	return pid, err
}

// Signal sends sig to the host process (and its process group). No reply
// is asked for: a forwarded Ctrl-C must not wait on the bus.
func (c *Client) Signal(pid uint32, sig syscall.Signal, group bool) error {
	return c.obj.Go(iface+".HostCommandSignal", dbus.FlagNoReplyExpected, nil, pid, uint32(sig), group).Err
}

// Exits delivers the bus's signals (HostCommandExited among them).
func (c *Client) Exits() <-chan *dbus.Signal { return c.signals }

// Status returns the wait status if sig is the exit of pid.
func Status(sig *dbus.Signal, pid uint32) (syscall.WaitStatus, bool) {
	if sig == nil || sig.Name != iface+".HostCommandExited" || len(sig.Body) != 2 {
		return 0, false
	}
	p, ok1 := sig.Body[0].(uint32)
	s, ok2 := sig.Body[1].(uint32)
	if !ok1 || !ok2 || p != pid {
		return 0, false
	}
	return syscall.WaitStatus(s), true
}

// Wait blocks until pid exits.
func (c *Client) Wait(pid uint32) (syscall.WaitStatus, error) {
	for sig := range c.signals {
		if ws, ok := Status(sig, pid); ok {
			return ws, nil
		}
	}
	return 0, fmt.Errorf("session bus closed while waiting for host pid %d", pid)
}
