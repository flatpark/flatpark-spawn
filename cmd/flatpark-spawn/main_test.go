package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// The tests run this test binary as the command: through links in a shim
// directory, as enable.sh makes them. The session bus is a socket of the
// test's that counts connections and refuses them, so the host is never
// reached; a count of 0 means the shim did not even try.
func TestMain(m *testing.M) {
	if os.Getenv("FPS_RUN_MAIN") == "1" {
		main()
	}
	os.Exit(m.Run())
}

type shimWorld struct {
	root, shims, sbin, other string
	bus                      *atomic.Int32
	env                      []string
}

func newShimWorld(t *testing.T) shimWorld {
	t.Helper()
	root := t.TempDir()
	w := shimWorld{root: root, shims: filepath.Join(root, "shims"), sbin: filepath.Join(root, "sbin"),
		other: filepath.Join(root, "other"), bus: new(atomic.Int32)}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{w.shims, w.sbin, w.other} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []string{filepath.Join(w.shims, "fps-both"), filepath.Join(w.shims, "fps-shimonly"),
		filepath.Join(w.other, "fps-compat"), filepath.Join(root, "flatpark-spawn")} {
		if err := os.Symlink(self, l); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\necho \"local: ${0##*/} $* PATH=$PATH DEPTH=${FLATPARK_SPAWN_SHIM_DEPTH-}\"\n"
	if err := os.WriteFile(filepath.Join(w.sbin, "fps-both"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "bus")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			w.bus.Add(1)
			c.Close()
		}
	}()
	w.env = []string{
		"FPS_RUN_MAIN=1", "HOME=" + root, "LC_ALL=C", "XDG_RUNTIME_DIR=" + filepath.Join(root, "run"),
		"PATH=" + w.shims + ":" + w.sbin + ":/usr/bin:/bin", "FLATPARK_SPAWN_SHIMS=" + w.shims,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + sock,
		"FLATPARK_SPAWN_CONFIG=" + filepath.Join(root, "config.yaml"),
	}
	return w
}

func (w shimWorld) run(t *testing.T, extra []string, name string, args ...string) (string, string, int) {
	t.Helper()
	path := name
	if !strings.ContainsRune(name, '/') {
		path = filepath.Join(w.shims, name)
	}
	cmd := exec.Command(path, args...)
	// A bare name: as execvp runs it, found in PATH with argv[0] the name;
	// only AT_EXECFN tells the shim directory.
	cmd.Args[0] = name
	cmd.Env = append(append([]string(nil), w.env...), extra...)
	cmd.Dir = "/" // not a sandbox-private working directory
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

func TestShimMode(t *testing.T) {
	w := newShimWorld(t)
	localOut := func(args string) string {
		return "local: fps-both " + args + " PATH=" + w.sbin + ":/usr/bin:/bin DEPTH=1\n"
	}
	for _, tc := range []struct {
		name    string
		extra   []string
		cmd     string
		args    []string
		out     string
		errHas  string
		code    int
		asked   bool // tried the host first
		command string
	}{
		{"host unreachable: the sandbox's copy, without the shim directory in PATH", nil,
			"fps-both", []string{"a"}, localOut("a"), "", 0, true, ""},
		{"by its full path (a program that found it on disk)", nil,
			"", []string{"b"}, localOut("b"), "", 0, true, "fps-both"},
		{"FLATPARK_SPAWN_LOCAL: local, the host not asked", []string{"FLATPARK_SPAWN_LOCAL=x:fps-both"},
			"fps-both", nil, localOut(""), "", 0, false, ""},
		{"FLATPARK_SPAWN_LOCAL without a local copy: not found, the host not asked", []string{"FLATPARK_SPAWN_LOCAL=fps-shimonly"},
			"fps-shimonly", nil, "", "fps-shimonly: command not found\n", 127, false, ""},
		{"private argument, the sandbox has it: local", nil,
			"fps-both", []string{"/app/share/x"}, localOut("/app/share/x"), "", 0, false, ""},
		{"private argument, only a shim here: tried on the host", nil,
			"fps-shimonly", []string{"/app/share/x"}, "", "fps-shimonly: command not found\n", 127, true, ""},
		{"nowhere: 127", nil,
			"fps-shimonly", nil, "", "fps-shimonly: command not found\n", 127, true, ""},
		{"its own local fallback calling it again: stops", []string{"FLATPARK_SPAWN_SHIM_DEPTH=1"},
			"fps-both", nil, "", "flatpark-spawn: shim fps-both: run again from its own local fallback", 126, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := w.bus.Load()
			name := tc.cmd
			if tc.command != "" {
				name = filepath.Join(w.shims, tc.command)
			}
			out, errs, code := w.run(t, tc.extra, name, tc.args...)
			if out != tc.out || code != tc.code || !strings.Contains(errs, tc.errHas) {
				t.Errorf("got exit %d\nstdout %q\nstderr %q\nwant exit %d, %q, stderr with %q", code, out, errs, tc.code, tc.out, tc.errHas)
			}
			if asked := w.bus.Load() > before; asked != tc.asked {
				t.Errorf("asked the host: %v, want %v", asked, tc.asked)
			}
		})
	}
	// A link outside the shim directory is host-spawn's shim, as in 0.1.
	_, errs, code := w.run(t, nil, filepath.Join(w.other, "fps-compat"))
	if code != 127 || !strings.Contains(errs, "flatpark-spawn: cannot run on the host") {
		t.Errorf("host-spawn shim: exit %d %q", code, errs)
	}
}

func TestCheckOnTheHost(t *testing.T) {
	w := newShimWorld(t)
	cfg := filepath.Join(w.root, "config.yaml")
	write := func(s string) {
		if err := os.WriteFile(cfg, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	self := filepath.Join(w.root, "flatpark-spawn")
	write("use_login_shell_path: false\ninclude_path: [" + w.sbin + ", /nonexistent-fps]\nshims: [fps-both, nope-fps]\n")
	out, errs, code := w.run(t, nil, self, "check")
	for _, want := range []string{"config: ~/config.yaml (found)", "use_login_shell_path   - off", "✓ ~/sbin",
		"✗ /nonexistent-fps", "✓ fps-both → ~/sbin/fps-both", "✗ nope-fps"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if code != 1 || errs != "" || w.bus.Load() != 0 {
		t.Errorf("exit %d, stderr %q, bus %d", code, errs, w.bus.Load())
	}
	write("use_login_shell_path: false\ninclude_path: [" + w.sbin + "]\nshims: [fps-both]\n")
	if out, _, code := w.run(t, nil, self, "check"); code != 0 {
		t.Errorf("all found: exit %d\n%s", code, out)
	}
	write("shims:\n  - fps-both\ninclude_paths: []\n")
	out, _, code = w.run(t, nil, self, "check")
	if code != 1 || !strings.Contains(out, cfg+`:3: unknown key "include_paths" (did you mean include_path?)`) {
		t.Errorf("error with its line: exit %d\n%s", code, out)
	}
	// `-- check` is a host command named check, as before.
	if _, errs, code := w.run(t, nil, self, "--", "check"); code != 127 || !strings.Contains(errs, "cannot run on the host") {
		t.Errorf("-- check: exit %d %q", code, errs)
	}
}

// TestMakeShimsKeepsTheOldSetOnFailure: enable.sh's --x-shims when the host
// cannot be asked (here: the session bus refuses) says so, exits non-zero
// (enable.sh ignores that and goes on) and leaves the shims as they were.
func TestMakeShimsKeepsTheOldSetOnFailure(t *testing.T) {
	w := newShimWorld(t)
	self := filepath.Join(w.root, "flatpark-spawn")
	_, errs, code := w.run(t, nil, self, "--x-shims", w.shims)
	if code != 1 || !strings.Contains(errs, "flatpark-spawn: shims not updated: ") {
		t.Errorf("exit %d, stderr %q", code, errs)
	}
	entries, err := os.ReadDir(w.shims)
	if err != nil || len(entries) != 2 {
		t.Errorf("old set: %v %v", entries, err)
	}
	if _, _, code := w.run(t, nil, self, "--x-shims", "relative"); code != 2 {
		t.Errorf("relative directory: exit %d", code)
	}
}
