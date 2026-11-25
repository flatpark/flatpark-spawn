package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/agent"
	"github.com/flatpark/flatpark-spawn/internal/names"
)

// ShimDir is this app's shim directory: FLATPARK_SPAWN_SHIMS (set by
// enable.sh, which puts it first in PATH), else the same path derived from
// XDG_RUNTIME_DIR and FLATPAK_ID; "" when neither is known.
func ShimDir() string {
	if d := os.Getenv(names.Env("SHIMS")); filepath.IsAbs(d) {
		return filepath.Clean(d)
	}
	run, id := os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("FLATPAK_ID")
	if !filepath.IsAbs(run) || id == "" || strings.ContainsRune(id, '/') {
		return ""
	}
	return filepath.Join(run, "app", id, names.Name, names.ShimDir)
}

// InShimDir reports whether path is <dir>/<valid name> (literally: no
// symlinks resolved), and returns the name.
func InShimDir(path, dir string) (string, bool) {
	if dir == "" || !filepath.IsAbs(path) {
		return "", false
	}
	base := filepath.Base(path)
	if filepath.Dir(filepath.Clean(path)) != filepath.Clean(dir) || !ValidName(base) {
		return "", false
	}
	return base, true
}

// WithoutDir is a PATH value without the entries naming dir.
func WithoutDir(path, dir string) string {
	if dir == "" {
		return path
	}
	var out []string
	for _, e := range filepath.SplitList(path) {
		if e != "" && filepath.Clean(e) == filepath.Clean(dir) {
			continue
		}
		out = append(out, e)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// LocalPath finds name in the sandbox's PATH, the shim directory left out:
// a shim is not the sandbox's own copy.
func LocalPath(name string) (string, bool) {
	if strings.ContainsRune(name, '/') {
		return "", false
	}
	return agent.LookPath(name, WithoutDir(os.Getenv("PATH"), ShimDir()))
}

// staleAfter: a <dir>.tmp-<pid> older than this was left by a run that
// died; a younger one may be another instance of the app starting now.
const staleAfter = time.Minute

// WriteShims makes dir hold exactly one symlink to target per name, and
// swaps it in for the old directory atomically (renameat2 RENAME_EXCHANGE:
// a program searching PATH meanwhile sees the old set or the new, never a
// missing directory). Without names the directory is removed.
func WriteShims(dir, target string, list []string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("shim directory %q is not absolute", dir)
	}
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if old, _ := filepath.Glob(dir + ".tmp-*"); len(old) > 0 {
		for _, o := range old {
			if st, err := os.Lstat(o); err == nil && time.Since(st.ModTime()) > staleAfter {
				_ = os.RemoveAll(o)
			}
		}
	}
	if len(list) == 0 {
		return os.RemoveAll(dir)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", dir, os.Getpid())
	_ = os.RemoveAll(tmp)
	if err := os.Mkdir(tmp, 0o700); err != nil {
		return err
	}
	for _, n := range list {
		if !ValidName(n) {
			_ = os.RemoveAll(tmp)
			return fmt.Errorf("%q is not a command name", n)
		}
		if err := os.Symlink(target, filepath.Join(tmp, n)); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	err := unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, dir, unix.RENAME_EXCHANGE)
	switch {
	case err == nil:
		// tmp now holds the old set.
	case errors.Is(err, unix.ENOENT):
		err = os.Rename(tmp, dir)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		// No RENAME_EXCHANGE on this file system: move the old one aside
		// first (a moment without the directory).
		aside := fmt.Sprintf("%s.old-%d", dir, os.Getpid())
		if err = os.Rename(dir, aside); err == nil {
			err = os.Rename(tmp, dir)
			_ = os.RemoveAll(aside)
		}
	}
	_ = os.RemoveAll(tmp)
	return err
}

// ShimNames lists dir (nil, false when it does not exist).
func ShimNames(dir string) ([]string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out, true
}
