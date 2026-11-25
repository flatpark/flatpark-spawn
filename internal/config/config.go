// Package config reads the host's flatpark-spawn configuration,
// $XDG_CONFIG_HOME/flatpark-spawn/config.yaml. Only the host agent reads it
// (and `flatpark-spawn check` run on the host): every app using
// flatpark-spawn shares the one file, whatever its own permissions.
//
//	use_login_shell_path: true    # the login shell's PATH first
//	include_path: [/opt/tools/bin] # then these, in order
//	exclude_path: [/snap/bin]      # never searched, nor run from
//	shims: [nvim]                  # links made in the sandbox at app start
//
// A missing file is the defaults. A file with an error is reported with
// its line by `check`; the agent then uses the defaults, so that a typo
// cannot keep every app's commands from reaching the host.
package config

import (
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/flatpark/flatpark-spawn/internal/names"
)

// FileName is the configuration's name below $XDG_CONFIG_HOME/<names.Name>.
const FileName = "config.yaml"

// Keys, in the order the documentation lists them.
const (
	KeyUseLogin = "use_login_shell_path"
	KeyInclude  = "include_path"
	KeyExclude  = "exclude_path"
	KeyShims    = "shims"
)

var keys = []string{KeyUseLogin, KeyInclude, KeyExclude, KeyShims}

// Config is a parsed configuration. The zero value is not the default; use
// Default.
type Config struct {
	UseLoginShellPath bool
	IncludePath       []string // absolute, cleaned, in order, as written
	ExcludePath       []string // absolute, cleaned
	Shims             []string // valid names, duplicates removed
}

// Default is the configuration of a missing file: 0.1's behaviour.
func Default() Config { return Config{UseLoginShellPath: true} }

// Problem is one error in the file, with its line (0: no line).
type Problem struct {
	Line int
	Msg  string
}

// Error is a configuration that cannot be used.
type Error struct {
	File     string
	Problems []Problem
}

func (e *Error) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = e.Format(p)
	}
	return strings.Join(parts, "; ")
}

// Format is "file:line: message" (or "file: message").
func (e *Error) Format(p Problem) string {
	if p.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, p.Line, p.Msg)
	}
	return fmt.Sprintf("%s: %s", e.File, p.Msg)
}

// Path is where the configuration is read from: FLATPARK_SPAWN_CONFIG when
// it is an absolute path (tests, trying a file out), else
// $XDG_CONFIG_HOME/flatpark-spawn/config.yaml, else ~/.config/....
func Path(env func(string) string) string {
	if p := env(names.Env("CONFIG")); filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	dir := env("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(env("HOME"), ".config")
	}
	return filepath.Join(dir, names.Name, FileName)
}

// Load reads file. A missing file is the defaults with exists false; a
// file that cannot be used is the defaults and an *Error (or the read
// error).
func Load(file, home string) (c Config, exists bool, err error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), false, nil
	}
	if err != nil {
		return Default(), true, &Error{File: file, Problems: []Problem{{Msg: err.Error()}}}
	}
	c, err = Parse(b, file, home)
	return c, true, err
}

var yamlLine = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)

// Parse parses the file's content; home expands a leading ~/.
func Parse(data []byte, file, home string) (Config, error) {
	c := Default()
	e := &Error{File: file}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return c, nil // empty file
		}
		e.Problems = append(e.Problems, syntaxProblem(err))
		return Default(), e
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		e.Problems = append(e.Problems, Problem{extra.Line, "more than one YAML document"})
	} else if !errors.Is(err, io.EOF) {
		e.Problems = append(e.Problems, syntaxProblem(err))
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	switch {
	case root.Kind == yaml.ScalarNode && root.Tag == "!!null":
		// only comments
	case root.Kind != yaml.MappingNode:
		e.Problems = append(e.Problems, Problem{root.Line, "the file must be a mapping of keys (" + strings.Join(keys, ", ") + ")"})
	default:
		seen := map[string]int{}
		for i := 0; i+1 < len(root.Content); i += 2 {
			k, v := root.Content[i], root.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				e.Problems = append(e.Problems, Problem{k.Line, "a key must be a plain word"})
				continue
			}
			if first, dup := seen[k.Value]; dup {
				e.Problems = append(e.Problems, Problem{k.Line, fmt.Sprintf("%q is set twice (first on line %d)", k.Value, first)})
				continue
			}
			seen[k.Value] = k.Line
			switch k.Value {
			case KeyUseLogin:
				if v.Kind != yaml.ScalarNode || v.Tag != "!!bool" {
					e.Problems = append(e.Problems, Problem{v.Line, KeyUseLogin + " must be true or false"})
					continue
				}
				b, _ := strconv.ParseBool(strings.ToLower(v.Value))
				c.UseLoginShellPath = b
			case KeyInclude, KeyExclude:
				list, probs := strings_(v, k.Value)
				e.Problems = append(e.Problems, probs...)
				var dirs []string
				for _, it := range list {
					d, msg := dir(it.Value, home)
					if msg != "" {
						e.Problems = append(e.Problems, Problem{it.Line, fmt.Sprintf("%s: %q %s", k.Value, it.Value, msg)})
						continue
					}
					dirs = append(dirs, d)
				}
				if k.Value == KeyInclude {
					c.IncludePath = dirs
				} else {
					c.ExcludePath = dirs
				}
			case KeyShims:
				list, probs := strings_(v, k.Value)
				e.Problems = append(e.Problems, probs...)
				have := map[string]bool{}
				for _, it := range list {
					if !ValidName(it.Value) {
						e.Problems = append(e.Problems, Problem{it.Line, fmt.Sprintf("shims: %q is not a command name (no '/', not . or ..)", it.Value)})
						continue
					}
					if !have[it.Value] {
						have[it.Value] = true
						c.Shims = append(c.Shims, it.Value)
					}
				}
			default:
				msg := fmt.Sprintf("unknown key %q", k.Value)
				if s := suggest(k.Value); s != "" {
					msg += fmt.Sprintf(" (did you mean %s?)", s)
				} else {
					msg += " (known: " + strings.Join(keys, ", ") + ")"
				}
				e.Problems = append(e.Problems, Problem{k.Line, msg})
			}
		}
	}
	if len(e.Problems) > 0 {
		sort.SliceStable(e.Problems, func(i, j int) bool { return e.Problems[i].Line < e.Problems[j].Line })
		return Default(), e
	}
	return c, nil
}

func syntaxProblem(err error) Problem {
	msg := err.Error()
	if m := yamlLine.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return Problem{n, "YAML syntax: " + m[2]}
	}
	return Problem{0, "YAML syntax: " + strings.TrimPrefix(msg, "yaml: ")}
}

// strings_ reads a list of scalars; an empty value is an empty list.
func strings_(v *yaml.Node, key string) ([]*yaml.Node, []Problem) {
	if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
		return nil, nil
	}
	if v.Kind != yaml.SequenceNode {
		return nil, []Problem{{v.Line, key + " must be a list (one \"- item\" per line, or [a, b])"}}
	}
	var out []*yaml.Node
	var probs []Problem
	for _, it := range v.Content {
		switch {
		case it.Kind != yaml.ScalarNode:
			probs = append(probs, Problem{it.Line, key + ": each item must be a single string"})
		case it.Tag == "!!null" || it.Tag == "!!bool":
			probs = append(probs, Problem{it.Line, fmt.Sprintf("%s: %q is not a string (quote it if you mean it)", key, it.Value)})
		default:
			out = append(out, it)
		}
	}
	return out, probs
}

// dir expands ~/ and checks that the result is absolute; it is cleaned but
// symlinks are not resolved (an exclude_path names that literal directory).
func dir(s, home string) (string, string) {
	switch {
	case s == "~" || strings.HasPrefix(s, "~/"):
		if !filepath.IsAbs(home) {
			return "", "uses ~ but HOME is not set"
		}
		s = home + s[1:]
	case strings.HasPrefix(s, "~"):
		return "", "is not supported (only ~/ for your own home)"
	case !filepath.IsAbs(s):
		return "", "is not an absolute path (start with / or ~/)"
	}
	return filepath.Clean(s), ""
}

// ValidName is a command name: what preload/route.c's fps_valid_name
// accepts (sandbox.ValidName uses this).
func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsRune(name, '/') && len(name) < 200
}

func suggest(k string) string {
	best, bestD := "", 4
	for _, known := range keys {
		if d := distance(strings.ToLower(k), known); d < bestD {
			best, bestD = known, d
		}
	}
	return best
}

// distance is the Levenshtein distance.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// Effective is the search path, as a PATH value:
//
//	dedupe((use_login_shell_path ? loginPath : []) ++ include_path) − exclude_path
//
// Duplicates keep their first place. Entries of the login PATH are kept as
// written (an empty or relative one too: the host command may rely on it;
// the agent's own lookup skips them) and compared in their cleaned form.
// With nothing configured it is the login PATH itself, unchanged, as in
// 0.1.
func (c Config) Effective(loginPath string) string {
	if c.UseLoginShellPath && len(c.IncludePath) == 0 && len(c.ExcludePath) == 0 {
		return loginPath
	}
	var in []string
	if c.UseLoginShellPath && loginPath != "" {
		in = append(in, filepath.SplitList(loginPath)...)
	}
	in = append(in, c.IncludePath...)
	excluded := map[string]bool{}
	for _, d := range c.ExcludePath {
		excluded[d] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		key := d
		if d != "" {
			key = filepath.Clean(d)
		}
		if seen[key] || excluded[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

// Excluded reports whether an absolute target lies in a directory of
// exclude_path or below one (no symlinks resolved).
func (c Config) Excluded(target string) bool {
	if !filepath.IsAbs(target) {
		return false
	}
	t := filepath.Clean(target)
	for _, d := range c.ExcludePath {
		if d == "/" || strings.HasPrefix(t, d+"/") {
			return true
		}
	}
	return false
}

// Fingerprint identifies the file's state for the sandbox's caches: its
// modification time and size, "" when there is no file.
func Fingerprint(file string) string {
	st, err := os.Stat(file)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	fmt.Fprintf(h, "%s %d %d", file, st.ModTime().UnixNano(), st.Size())
	return strconv.FormatUint(h.Sum64(), 16)
}
