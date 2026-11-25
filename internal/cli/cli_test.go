package cli

import (
	"reflect"
	"testing"
)

func TestKindOf(t *testing.T) {
	for argv0, want := range map[string]Kind{
		"flatpark-spawn": Ours,
		"/app/lib/flatpark-spawn/bin/flatpark-spawn": Ours,
		"host-spawn":          Compat,
		"/app/bin/host-spawn": Compat,
		"host-spawn-x86_64":   Compat, // upstream matches the prefix
		"podman":              Shim,
		"/app/bin/docker":     Shim,
	} {
		if got := KindOf(argv0); got != want {
			t.Errorf("%s: got %d, want %d", argv0, got, want)
		}
	}
}

func TestHostSpawnOptions(t *testing.T) {
	o, err := Parse([]string{"host-spawn", "-no-pty", "-cwd", "/home/me", "-env", "TERM,FOO", "ls", "-la", "--color"})
	if err != nil {
		t.Fatal(err)
	}
	want := Options{Kind: Compat, NoPty: true, Cwd: "/home/me", Env: "TERM,FOO", EnvSet: true, Args: []string{"ls", "-la", "--color"}}
	if !reflect.DeepEqual(o, want) {
		t.Fatalf("got %+v\nwant %+v", o, want)
	}
	// Double dashes, as Go's flag package accepts and VS Code configs use.
	o, err = Parse([]string{"host-spawn", "--pty", "--env=PATH", "bash"})
	if err != nil || !o.Pty || o.Env != "PATH" || !reflect.DeepEqual(o.Args, []string{"bash"}) {
		t.Fatalf("%+v %v", o, err)
	}
	// Upstream's default -env is TERM.
	o, _ = Parse([]string{"host-spawn"})
	if o.Env != "TERM" || o.EnvSet || len(o.Args) != 0 {
		t.Fatalf("%+v", o)
	}
	if got := o.DefaultCommand(); !reflect.DeepEqual(got, []string{"sh", "-c", "$SHELL"}) {
		t.Fatalf("no command opens upstream's shell: %v", got)
	}
	o, _ = Parse([]string{"host-spawn", "-version"})
	if !o.Version {
		t.Fatal("-version")
	}
	if _, err := Parse([]string{"host-spawn", "-bogus"}); err == nil {
		t.Fatal("unknown options are errors")
	}
	o, _ = Parse([]string{"host-spawn", "-h"})
	if !o.Help || o.Usage == "" {
		t.Fatal("-h prints usage")
	}
}

func TestOurOptions(t *testing.T) {
	o, err := Parse([]string{"flatpark-spawn", "-cwd", "/x", "git", "-c", "a=b", "status"})
	if err != nil || o.Kind != Ours || o.Cwd != "/x" || o.Env != "" ||
		!reflect.DeepEqual(o.Args, []string{"git", "-c", "a=b", "status"}) {
		t.Fatalf("%+v %v", o, err)
	}
	if o.DefaultCommand() != nil {
		t.Fatal("ours opens the login shell through the agent")
	}
	// Options stop at the command; "--" works too.
	o, _ = Parse([]string{"flatpark-spawn", "--", "-weird-name"})
	if !reflect.DeepEqual(o.Args, []string{"-weird-name"}) {
		t.Fatalf("%v", o.Args)
	}
}

func TestShim(t *testing.T) {
	o, err := Parse([]string{"/app/bin/podman", "-pty", "ps"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != Shim || o.Pty || o.Env != "TERM" || !reflect.DeepEqual(o.Args, []string{"podman", "-pty", "ps"}) {
		t.Fatalf("a shim passes every argument through: %+v", o)
	}
}

func TestWantPty(t *testing.T) {
	ours, _ := Parse([]string{"flatpark-spawn", "bash"})
	compat, _ := Parse([]string{"host-spawn", "bash"})
	blocked, _ := Parse([]string{"host-spawn", "xdg-open", "x"})
	forced, _ := Parse([]string{"flatpark-spawn", "-pty", "cat"})
	never, _ := Parse([]string{"host-spawn", "-no-pty", "bash"})
	for _, tc := range []struct {
		name          string
		o             Options
		stdin, stdout bool
		want          bool
	}{
		{"ours, terminal", ours, true, true, true},
		{"ours, piped stdout", ours, true, false, false},
		{"ours, piped stdin", ours, false, true, false},
		{"host-spawn, piped stdin", compat, false, true, true},
		{"host-spawn, piped stdout", compat, true, false, false},
		{"host-spawn, blocklisted", blocked, true, true, false},
		{"-pty", forced, false, false, true},
		{"-no-pty", never, true, true, false},
	} {
		if got := tc.o.WantPty(tc.stdin, tc.stdout); got != tc.want {
			t.Errorf("%s: got %v", tc.name, got)
		}
	}
}
