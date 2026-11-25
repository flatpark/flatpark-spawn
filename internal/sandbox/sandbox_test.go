package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flatpark/flatpark-spawn/internal/proto"
)

func TestRecord(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	c := Cache{Dir: root, Cwd: "/home/me/project", Now: time.Now}
	dir := c.CwdDir()
	exists := func(sub, name string) bool {
		_, err := os.Stat(filepath.Join(dir, sub, name))
		return err == nil
	}
	c.Record("git", true, "g1")
	c.Record("apptool", false, "g1")
	if !exists("host", "git") || !exists("none", "apptool") {
		t.Fatal("entries not written")
	}
	// An answer replaces the opposite one.
	c.Record("git", false, "g1")
	if exists("host", "git") || !exists("none", "git") {
		t.Fatal("git should now be negative only")
	}
	// A new generation empties both.
	c.Record("node", true, "g2")
	if exists("none", "git") || exists("none", "apptool") || !exists("host", "node") {
		t.Fatal("generation change must drop older entries")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "gen")); string(b) != "g2" {
		t.Fatalf("gen file %q", b)
	}
	// No generation (an error reply) keeps what is there.
	c.Record("x", false, "")
	if !exists("host", "node") {
		t.Fatal("an empty generation must not clear")
	}
	// Nothing is written for odd names or without a directory.
	c.Record("../evil", true, "g2")
	c.Record("..", true, "g2")
	if entries, _ := os.ReadDir(filepath.Join(dir, "host")); len(entries) != 1 || entries[0].Name() != "node" {
		t.Fatalf("invalid name written: %v", entries)
	}
	if exists("", "evil") || exists("..", "evil") {
		t.Fatal("path escape")
	}
	Cache{}.Record("git", true, "g")           // no dir: no-op
	Cache{Dir: "rel"}.Record("git", true, "g") // relative: no-op
	if _, err := os.Stat("rel"); err == nil {
		t.Fatal("relative cache dir used")
	}
}

func TestLookupForDirectStarts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	now := time.Unix(1_800_000_000, 0)
	c := Cache{Dir: dir, Cwd: "/home/me/project", Now: func() time.Time { return now }}
	c.RecordReply("git", proto.Reply{Kind: proto.Found, Gen: "g", Path: "/usr/bin/git", LoginPath: "/a:/usr/bin", Shell: "/bin/zsh"})
	hit, ok := c.Lookup("git")
	if !ok || hit.Path != "/usr/bin/git" || hit.LoginPath != "/a:/usr/bin" || hit.Shell != "/bin/zsh" {
		t.Fatalf("%+v %v", hit, ok)
	}
	now = now.Add(PosTTL)
	if _, ok := c.Lookup("git"); ok {
		t.Fatal("an entry as old as PosTTL goes through the agent again")
	}
	// The agent answers again: the entry is fresh once more.
	c.RecordReply("git", proto.Reply{Kind: proto.Found, Gen: "g", Path: "/usr/bin/git", LoginPath: "/a:/usr/bin"})
	if _, ok := c.Lookup("git"); !ok {
		t.Fatal("re-recorded entry must be fresh")
	}
	// An unchanged recent answer is not rewritten.
	entry := filepath.Join(c.HostDir(), "git")
	st1, _ := os.Stat(entry)
	now = now.Add(time.Second)
	c.RecordReply("git", proto.Reply{Kind: proto.Found, Gen: "g", Path: "/usr/bin/git", LoginPath: "/a:/usr/bin"})
	if st2, _ := os.Stat(entry); !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("rewritten although unchanged")
	}
	// Entries written by Record (no path) or naming another file are not hits.
	c.Record("bare", true, "g")
	if _, ok := c.Lookup("bare"); ok {
		t.Fatal("an entry without a path is not a hit")
	}
	c.RecordReply("ls", proto.Reply{Kind: proto.Found, Gen: "g", Path: "/usr/bin/other", LoginPath: "/usr/bin"})
	if _, ok := c.Lookup("ls"); ok {
		t.Fatal("a path whose basename is not the name is not a hit")
	}
	c.RecordReply("gone", proto.Reply{Kind: proto.None, Gen: "g"})
	if _, ok := c.Lookup("gone"); ok {
		t.Fatal("a negative answer is not a hit")
	}
}

// What the host has depends on the directory (mise, direnv): an answer for
// one working directory never answers for another.
func TestCachePerDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	a := Cache{Dir: root, Cwd: "/home/me/proj-a", Now: now}
	b := Cache{Dir: root, Cwd: "/home/me/proj-b", Now: now}
	a.RecordReply("node", proto.Reply{Kind: proto.Found, Gen: "ga", Path: "/mise/node/20/bin/node", LoginPath: "/mise/node/20/bin:/usr/bin"})
	b.RecordReply("node", proto.Reply{Kind: proto.Found, Gen: "gb", Path: "/mise/node/22/bin/node", LoginPath: "/mise/node/22/bin:/usr/bin"})
	ha, okA := a.Lookup("node")
	hb, okB := b.Lookup("node")
	if !okA || !okB || ha.Path != "/mise/node/20/bin/node" || hb.Path != "/mise/node/22/bin/node" || ha.LoginPath == hb.LoginPath {
		t.Fatalf("a=%+v b=%+v", ha, hb)
	}
	if a.HostDir() == b.HostDir() {
		t.Fatal("same preload directory for two cwds")
	}
	a.RecordReply("only-a", proto.Reply{Kind: proto.Found, Gen: "ga", Path: "/a/bin/only-a", LoginPath: "/a/bin"})
	if _, ok := b.Lookup("only-a"); ok {
		t.Fatal("proj-a's answer used in proj-b")
	}
	// One generation change clears only its own directory.
	a.Record("x", false, "ga2")
	if _, ok := b.Lookup("node"); !ok {
		t.Fatal("proj-b lost its entries to proj-a's generation change")
	}
	// The per-directory part is bounded.
	for i := 0; i < MaxDirs+5; i++ {
		Cache{Dir: root, Cwd: fmt.Sprintf("/p/%d", i), Now: now}.Record("y", false, "g")
	}
	entries, _ := os.ReadDir(filepath.Dir(a.CwdDir()))
	if len(entries) > MaxDirs {
		t.Fatalf("%d directories kept", len(entries))
	}
}

func TestDirectEnvSendsOnlyDifferences(t *testing.T) {
	session := proto.ParseHashes(proto.EnvHashes([]string{"LANG=C.UTF-8", "HOME=/home/me", "EDITOR=vim"}))
	hit := Hit{LoginPath: "/login:/usr/bin", Shell: "/bin/zsh", Session: session}
	env := map[string]string{"LANG": "C.UTF-8", "EDITOR": "nano", "CI": "1"}
	got := DirectEnv(env, hit)
	want := map[string]string{"EDITOR": "nano", "CI": "1", "PATH": "/login:/usr/bin", "SHELL": "/bin/zsh"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q in %v", k, got[k], got)
		}
	}
	// No fingerprint (older cache): everything is sent.
	hit.Session = nil
	if got := DirectEnv(env, hit); len(got) != 5 || got["LANG"] != "C.UTF-8" {
		t.Fatalf("without a fingerprint: %v", got)
	}
	// The session has SHELL: it is not overridden.
	hit.Session = proto.ParseHashes(proto.EnvHashes([]string{"SHELL=/bin/bash"}))
	if _, ok := DirectEnv(map[string]string{}, hit)["SHELL"]; ok {
		t.Fatal("SHELL set although the session has one")
	}
}

func TestHostEnv(t *testing.T) {
	in := []string{
		"PATH=/app/bin:/usr/bin", "HOME=/home/me", "TERM=xterm-256color",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "CI=1",
		"LD_PRELOAD=/app/lib/flatpark-spawn/lib/libflatpark-spawn.so",
		"ZYPAK_LD_PRELOAD=x", "FLATPAK_ID=org.x", "FLATPARK_SPAWN_ENABLE=1",
		"BASH_FUNC_command_not_found_handle%%=() { :; }",
		"XDG_CONFIG_HOME=/home/me/.var/app/org.x/config", "XDG_DATA_DIRS=/app/share:/usr/share",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/flatpak/bus", "DISPLAY=:99",
		"GIT_ASKPASS=/app/bin/askpass", "NODE_OPTIONS=--require /app/lib/x.js",
		"EDITOR=/usr/bin/vim", "TMPDIR=/run/user/1000/app/org.x", "OLD_TMP=/tmp/x",
		"PYTHONPATH=/app/lib/python", "ELECTRON_RUN_AS_NODE=1", "container=flatpak",
		"NOEQUALS",
	}
	got := HostEnv(in, false)
	want := map[string]string{
		"TERM": "xterm-256color", "GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C", "CI": "1",
		"EDITOR": "/usr/bin/vim", "TMPDIR": "/run/user/1000/app/org.x",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: got %q in %v", k, got[k], got)
		}
	}
	if HostEnv([]string{"OLD_TMP=/tmp/x"}, true)["OLD_TMP"] != "/tmp/x" {
		t.Fatal("/tmp values pass when /tmp is shared")
	}
}

func TestCompatEnv(t *testing.T) {
	t.Setenv("FPS_A", "1")
	t.Setenv("FPS_EMPTY", "")
	got := CompatEnv("FPS_A,FPS_EMPTY,FPS_UNSET,")
	if len(got) != 2 || got["FPS_A"] != "1" || got["FPS_EMPTY"] != "" {
		t.Fatalf("%v", got)
	}
}

func TestTmpShared(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "info")
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for content, want := range map[string]bool{
		"[Context]\nfilesystems=/tmp;\n":                    true,
		"[Context]\nfilesystems=xdg-config/git:ro;/tmp:rw;": true,
		"[Context]\nfilesystems=host;\n":                    false, // host does not share /tmp
		"[Context]\nfilesystems=/tmpfoo;\n":                 false,
		"[Application]\nfilesystems=/tmp;\n":                false,
		"":                                                  false,
	} {
		if got := TmpShared(write(content)); got != want {
			t.Errorf("%q: got %v", content, got)
		}
	}
	if TmpShared(filepath.Join(dir, "missing")) {
		t.Fatal("missing file")
	}
}
