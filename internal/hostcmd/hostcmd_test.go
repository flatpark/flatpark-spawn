package hostcmd

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestStatus(t *testing.T) {
	exit := &dbus.Signal{Name: iface + ".HostCommandExited", Body: []any{uint32(7), uint32(42 << 8)}}
	if ws, ok := Status(exit, 7); !ok || ws.ExitStatus() != 42 {
		t.Fatalf("%v %v", ws, ok)
	}
	if _, ok := Status(exit, 8); ok {
		t.Fatal("another pid's exit")
	}
	killed := &dbus.Signal{Name: iface + ".HostCommandExited", Body: []any{uint32(7), uint32(15)}}
	if ws, ok := Status(killed, 7); !ok || !ws.Signaled() || ws.Signal() != 15 {
		t.Fatalf("signaled: %v", ws)
	}
	for _, s := range []*dbus.Signal{nil, {Name: "org.freedesktop.DBus.NameAcquired", Body: []any{"x"}}, {Name: iface + ".HostCommandExited", Body: []any{"7", uint32(0)}}} {
		if _, ok := Status(s, 7); ok {
			t.Fatalf("%v is not an exit", s)
		}
	}
}

func TestMatchRule(t *testing.T) {
	if MatchRule != "type='signal',interface='org.freedesktop.Flatpak.Development',member='HostCommandExited'" {
		t.Fatal(MatchRule)
	}
}
