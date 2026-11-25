// Package sandbox is the sandbox side of the command: the lookup cache the
// preload library reads, the environment handed to the host, and the
// HostCommand session that runs the agent.
package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/flatpark/flatpark-spawn/internal/agent"
	"github.com/flatpark/flatpark-spawn/internal/names"
	"github.com/flatpark/flatpark-spawn/internal/proto"
)

// PosTTL is how long a positive answer is used to start the command directly.
// After that the agent resolves the name again (and may refresh the login
// PATH, see agent.MaxAge).
const PosTTL = 30 * time.Second

// MaxDirs bounds the per-directory entries; the oldest go first.
const MaxDirs = 256

// Cache records what the host has, so that the preload library can route a
// bare name without asking the host each time. What the host has depends
// on the working directory (mise, direnv and the like change PATH per
// project), so everything but the session fingerprint is kept per cwd:
//
//	<dir>/d/<key>/host/<name>  the host has it here: forward. Holds the host
//	                           executable; while younger than PosTTL the
//	                           command starts that file directly, without the
//	                           agent. The preload library only checks that it
//	                           exists (no expiry: a stale entry costs one
//	                           refused direct start and an agent run)
//	<dir>/d/<key>/none/<name>  the host lacks it here: run locally; trusted
//	                           while younger than FPS_NEG_TTL_* (route.h)
//	<dir>/d/<key>/gen          the host generation of the last answer here;
//	                           a different one empties host/ and none/
//	<dir>/d/<key>/env          login PATH (for this cwd) and shell
//	<dir>/session              fingerprint of the host session's environment
//
// <key> is agent.Key of the physical working directory (getcwd), the same
// value preload/route.c computes. <dir> is $XDG_RUNTIME_DIR/app/$FLATPAK_ID/
// flatpark-spawn, set by enable.sh as FLATPARK_SPAWN_CACHE. Without it
// nothing is cached and every unknown name is looked up.
type Cache struct {
	Dir string
	Cwd string
	Now func() time.Time
}

// Getwd is the physical working directory, as getcwd(3) returns it (and
// the preload library sees it). os.Getwd prefers $PWD, which may name the
// same directory through a symlink and then hash differently.
func Getwd() string {
	if d, err := unix.Getwd(); err == nil {
		return d
	}
	return "/"
}

// CacheFromEnv returns the cache named by the environment, for the
// current directory.
func CacheFromEnv() Cache {
	return Cache{Dir: os.Getenv(names.Env("CACHE")), Cwd: Getwd(), Now: time.Now}
}

// ValidName mirrors fps_valid_name in preload/route.c.
func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/') && len(name) < 200
}

func (c Cache) usable() bool { return c.Dir != "" && filepath.IsAbs(c.Dir) && c.Cwd != "" }

// CwdDir is this working directory's part of the cache.
func (c Cache) CwdDir() string {
	return filepath.Join(c.Dir, names.CacheCwd, agent.Key(c.Cwd))
}

// HostDir and NoneDir are what the preload library reads for this cwd.
func (c Cache) HostDir() string { return filepath.Join(c.CwdDir(), names.CacheHost) }
func (c Cache) NoneDir() string { return filepath.Join(c.CwdDir(), names.CacheNone) }

// Record stores one answer from the host.
func (c Cache) Record(name string, onHost bool, gen string) { c.record(name, onHost, gen, "") }

// RecordReply stores an agent reply about name.
func (c Cache) RecordReply(name string, r proto.Reply) {
	switch r.Kind {
	case proto.Found:
		c.record(name, true, r.Gen, r.Path)
		if r.LoginPath != "" && c.usable() {
			c.store(filepath.Join(c.CwdDir(), "env"), r.LoginPath+"\n"+r.Shell+"\n")
		}
	case proto.None:
		c.record(name, false, r.Gen, "")
	}
	if r.Session != "" && c.usable() {
		c.store(filepath.Join(c.Dir, "session"), r.Session)
	}
}

// store writes a small file atomically, unless it already says that.
func (c Cache) store(file, content string) {
	if old, err := os.ReadFile(file); err == nil && string(old) == content {
		return
	}
	if os.MkdirAll(filepath.Dir(file), 0o700) != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d", file, os.Getpid())
	if os.WriteFile(tmp, []byte(content), 0o600) == nil {
		_ = os.Rename(tmp, file)
	}
}

// Hit is a fresh positive answer: enough to start the command directly.
type Hit struct {
	Path, LoginPath, Shell string
	// Session: name -> value fingerprint of the host session's environment.
	Session map[string]string
}

// Lookup returns a positive answer for this cwd younger than PosTTL.
func (c Cache) Lookup(name string) (Hit, bool) {
	if !c.usable() || !ValidName(name) || c.Now == nil {
		return Hit{}, false
	}
	entry := filepath.Join(c.HostDir(), name)
	st, err := os.Stat(entry)
	if err != nil {
		return Hit{}, false
	}
	if age := c.Now().Sub(st.ModTime()); age < 0 || age >= PosTTL {
		return Hit{}, false
	}
	path, err := os.ReadFile(entry)
	if err != nil || !filepath.IsAbs(string(path)) || filepath.Base(string(path)) != name {
		return Hit{}, false
	}
	env, err := os.ReadFile(filepath.Join(c.CwdDir(), "env"))
	if err != nil {
		return Hit{}, false
	}
	lines := strings.SplitN(string(env), "\n", 3)
	if len(lines) < 2 || lines[0] == "" {
		return Hit{}, false
	}
	hit := Hit{Path: string(path), LoginPath: lines[0], Shell: lines[1]}
	if b, err := os.ReadFile(filepath.Join(c.Dir, "session")); err == nil {
		hit.Session = proto.ParseHashes(string(b))
	}
	return hit, true
}

func (c Cache) record(name string, onHost bool, gen, content string) {
	if !c.usable() || !ValidName(name) {
		return
	}
	dir := c.CwdDir()
	if _, err := os.Stat(dir); err != nil {
		if os.MkdirAll(dir, 0o700) != nil {
			return
		}
		agent.Prune(filepath.Dir(dir), MaxDirs)
	}
	hostDir, noneDir := c.HostDir(), c.NoneDir()
	sameGen := gen == ""
	if gen != "" {
		genFile := filepath.Join(dir, "gen")
		old, err := os.ReadFile(genFile)
		sameGen = err == nil && string(old) == gen
		if !sameGen {
			_ = os.RemoveAll(hostDir)
			_ = os.RemoveAll(noneDir)
			c.store(genFile, gen)
		}
	}
	add, del := noneDir, hostDir
	if onHost {
		add, del = hostDir, noneDir
	}
	entry := filepath.Join(add, name)
	// Nothing to write for an unchanged, recent positive answer.
	if onHost && sameGen && c.Now != nil {
		if st, err := os.Stat(entry); err == nil && c.Now().Sub(st.ModTime()) < PosTTL/2 {
			if old, err := os.ReadFile(entry); err == nil && string(old) == content {
				return
			}
		}
	}
	_ = os.Remove(filepath.Join(del, name))
	if os.MkdirAll(add, 0o700) != nil {
		return
	}
	if os.WriteFile(entry, []byte(content), 0o600) == nil && c.Now != nil {
		now := c.Now()
		_ = os.Chtimes(entry, now, now)
	}
}
