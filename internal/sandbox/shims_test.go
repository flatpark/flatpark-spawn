package sandbox

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func links(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("%s is not a link: %v", e.Name(), err)
		}
		m[e.Name()] = target
	}
	return m
}

func TestWriteShims(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "flatpark-spawn", "shims")
	const target = "/app/lib/flatpark-spawn/bin/flatpark-spawn"
	if err := WriteShims(dir, target, []string{"nvim", "rg"}); err != nil {
		t.Fatal(err)
	}
	if got := links(t, dir); !reflect.DeepEqual(got, map[string]string{"nvim": target, "rg": target}) {
		t.Fatalf("first set: %v", got)
	}
	// Replaced as a whole: rg (no longer configured or found) goes away.
	before, _ := os.Stat(dir)
	if err := WriteShims(dir, target, []string{"nvim", "fd"}); err != nil {
		t.Fatal(err)
	}
	if got := links(t, dir); !reflect.DeepEqual(got, map[string]string{"nvim": target, "fd": target}) {
		t.Fatalf("second set: %v", got)
	}
	after, _ := os.Stat(dir)
	if os.SameFile(before, after) {
		t.Error("a new directory was swapped in, not the old one edited")
	}
	// Nothing left next to it but the directory itself.
	if left, _ := filepath.Glob(dir + ".*"); len(left) != 0 {
		t.Errorf("left over: %v", left)
	}
	// A stale temporary directory of a run that died is cleaned up; a
	// fresh one (another instance starting now) is left alone.
	stale, fresh := dir+".tmp-999999", dir+".tmp-999998"
	for _, d := range []string{stale, fresh} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteShims(dir, target, []string{"nvim"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale temporary directory kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh temporary directory removed")
	}
	// No names: no directory (enable.sh then leaves PATH alone).
	if err := WriteShims(dir, target, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("empty set: %v", err)
	}
	if err := WriteShims(dir, target, []string{"a/b"}); err == nil {
		t.Error("an invalid name must fail")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("a failed run leaves the old state")
	}
	if err := WriteShims("rel/shims", target, []string{"x"}); err == nil {
		t.Error("a relative directory must fail")
	}
}

func TestShimPaths(t *testing.T) {
	for _, tc := range []struct {
		path, dir, name string
		ok              bool
	}{
		{"/run/a/shims/nvim", "/run/a/shims", "nvim", true},
		{"/run/a/shims/nvim", "/run/a/shims/", "nvim", true},
		{"/run/a//shims/nvim", "/run/a/shims", "nvim", true},
		{"/run/a/shims/sub/nvim", "/run/a/shims", "", false},
		{"/run/a/shims", "/run/a/shims", "", false},
		{"nvim", "/run/a/shims", "", false},
		{"/run/a/shims/nvim", "", "", false},
		{"/run/a/shims/..", "/run/a/shims", "", false},
	} {
		name, ok := InShimDir(tc.path, tc.dir)
		if name != tc.name || ok != tc.ok {
			t.Errorf("InShimDir(%q, %q) = %q %v", tc.path, tc.dir, name, ok)
		}
	}
	if got := WithoutDir("/s:/usr/bin:/s/::/bin", "/s"); got != "/usr/bin::/bin" {
		t.Errorf("WithoutDir: %q", got)
	}
	t.Setenv("FLATPARK_SPAWN_SHIMS", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("FLATPAK_ID", "org.x.App")
	if got := ShimDir(); got != "/run/user/1000/app/org.x.App/flatpark-spawn/shims" {
		t.Errorf("derived: %q", got)
	}
	t.Setenv("FLATPARK_SPAWN_SHIMS", "/elsewhere/shims/")
	if got := ShimDir(); got != "/elsewhere/shims" {
		t.Errorf("from the environment: %q", got)
	}
	t.Setenv("FLATPARK_SPAWN_SHIMS", "")
	t.Setenv("FLATPAK_ID", "")
	if got := ShimDir(); got != "" {
		t.Errorf("unknown: %q", got)
	}
}

func TestLocalPathSkipsShims(t *testing.T) {
	root := t.TempDir()
	shims, bin := filepath.Join(root, "shims"), filepath.Join(root, "bin")
	for _, d := range []string{shims, bin} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(shims, "both"), filepath.Join(shims, "shimonly"), filepath.Join(bin, "both")} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("FLATPARK_SPAWN_SHIMS", shims)
	t.Setenv("PATH", shims+":"+bin)
	if p, ok := LocalPath("both"); !ok || p != filepath.Join(bin, "both") {
		t.Errorf("both: %q %v", p, ok)
	}
	if p, ok := LocalPath("shimonly"); ok {
		t.Errorf("a shim is not a local copy: %q", p)
	}
	names, ok := ShimNames(shims)
	sort.Strings(names)
	if !ok || !reflect.DeepEqual(names, []string{"both", "shimonly"}) {
		t.Errorf("ShimNames: %v %v", names, ok)
	}
}
