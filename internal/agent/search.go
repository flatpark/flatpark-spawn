package agent

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/flatpark/flatpark-spawn/internal/config"
)

// Search is where the agent looks for commands: the login PATH of the
// command's directory (LoginPath) combined with the configuration
// (config.Config.Effective). It is the one place both are put together:
// lookups, the PATH a command runs with, shims and `check` all use it.
type Search struct {
	LP  *LoginPath
	Cfg config.Config
	// FP is config.Fingerprint of the configuration file ("" without one),
	// part of every generation: a changed file empties the sandbox's
	// caches at the next answer.
	FP string
}

// login returns the login PATH when the configuration uses it, without
// running a shell when it does not.
func (s *Search) login() (path string, at time.Time, refreshed bool) {
	if !s.Cfg.UseLoginShellPath {
		return "", s.LP.Now(), true
	}
	return s.LP.get()
}

// Path is the search path (PATH value), running the login shell when
// nothing is cached.
func (s *Search) Path() string {
	p, _, _ := s.login()
	return s.Cfg.Effective(p)
}

// Resolve finds name. A miss refreshes the login PATH once, as
// LoginPath.Resolve does. It returns the executable, the search path, and
// whether it was found.
func (s *Search) Resolve(name string) (string, string, bool) {
	login, at, refreshed := s.login()
	path := s.Cfg.Effective(login)
	if full, ok := LookPath(name, path); ok {
		return full, path, true
	}
	if !refreshed && s.LP.Now().Sub(at) >= RefreshMin {
		path = s.Cfg.Effective(s.LP.Refresh())
		if full, ok := LookPath(name, path); ok {
			return full, path, true
		}
	}
	return "", path, false
}

// Gen is the generation of an answer found with path.
func (s *Search) Gen(path string) string {
	g := Generation(path)
	if s.FP != "" {
		g += "." + s.FP
	}
	return g
}

// Report is what `check` shows and what makes the shims: the configuration
// (or its errors), the search path in the search's directory, and where
// each shim name resolves.
func (s *Search) Report(file string, exists bool, loadErr error) config.Report {
	r := config.Report{File: file, Exists: exists, Cwd: s.LP.Cwd, UseLogin: s.Cfg.UseLoginShellPath}
	if loadErr != nil {
		var ce *config.Error
		if errors.As(loadErr, &ce) {
			for _, p := range ce.Problems {
				r.Errors = append(r.Errors, ce.Format(p))
			}
		} else {
			r.Errors = []string{loadErr.Error()}
		}
		return r
	}
	login, _, _ := s.login()
	for _, d := range filepath.SplitList(login) {
		if filepath.IsAbs(d) {
			r.LoginDirs++
		}
	}
	for _, d := range s.Cfg.IncludePath {
		st, err := os.Stat(d)
		r.Include = append(r.Include, config.Dir{Path: d, Exists: err == nil && st.IsDir()})
	}
	r.Exclude = s.Cfg.ExcludePath
	r.Effective = s.Cfg.Effective(login)
	for _, name := range s.Cfg.Shims {
		full, path, ok := s.Resolve(name)
		sh := config.Shim{Name: name}
		if ok {
			sh.Path = full
		} else {
			for _, d := range filepath.SplitList(path) {
				if filepath.IsAbs(d) {
					sh.Checked = append(sh.Checked, d)
				}
			}
		}
		r.Shims = append(r.Shims, sh)
	}
	return r
}
