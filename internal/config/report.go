package config

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Report is what the host knows about the configuration, for `check` and
// for making shims. The agent fills it in on the host and sends it to the
// sandbox as JSON; Render prints it.
type Report struct {
	File   string   `json:"file"`
	Exists bool     `json:"exists"`
	Errors []string `json:"errors,omitempty"` // "file:line: message"

	UseLogin  bool     `json:"use_login"`
	LoginDirs int      `json:"login_dirs"` // entries of the login PATH used
	Include   []Dir    `json:"include,omitempty"`
	Exclude   []string `json:"exclude,omitempty"`
	Effective string   `json:"effective"` // the search path (PATH value)
	Shims     []Shim   `json:"shims,omitempty"`
	Cwd       string   `json:"cwd"`
}

// Dir is a directory of include_path and whether the host has it.
type Dir struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// Shim is one name of shims: the host executable it resolves to, or ""
// with the directories that were searched.
type Shim struct {
	Name    string   `json:"name"`
	Path    string   `json:"path,omitempty"`
	Checked []string `json:"checked,omitempty"`
}

// Encode and DecodeReport carry a Report over the control socket.
func (r Report) Encode() string { b, _ := json.Marshal(r); return string(b) }

func DecodeReport(s string) (Report, error) {
	var r Report
	err := json.Unmarshal([]byte(s), &r)
	return r, err
}

// Found returns the names that resolve on the host.
func (r Report) Found() []string {
	var out []string
	for _, s := range r.Shims {
		if s.Path != "" {
			out = append(out, s.Name)
		}
	}
	return out
}

// Sandbox is the shim directory as `check` finds it inside the app: its
// path and the names in it (nil Names with Missing: no directory).
type Sandbox struct {
	Dir     string
	Names   []string
	Missing bool
}

// Render prints the report as `check` does and returns its exit status:
// 1 for a configuration error or any ✗, else 0 (warnings included).
func Render(w io.Writer, r Report, home string, sb *Sandbox) int {
	status := 0
	short := func(p string) string {
		if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
			return "~" + p[len(home):]
		}
		return p
	}
	state := "found"
	if !r.Exists {
		state = "not found, using the defaults"
	}
	fmt.Fprintf(w, "config: %s (%s)\n", short(r.File), state)
	if len(r.Errors) > 0 {
		fmt.Fprintln(w)
		for _, e := range r.Errors {
			fmt.Fprintf(w, "✗ %s\n", e)
		}
		fmt.Fprintln(w, "\nThe file is ignored until this is fixed: commands are looked up as without a configuration.")
		return 1
	}
	fmt.Fprintln(w)
	if r.UseLogin {
		fmt.Fprintf(w, "use_login_shell_path   ✓ %d directories (login shell in %s)\n", r.LoginDirs, short(r.Cwd))
	} else {
		fmt.Fprintln(w, "use_login_shell_path   - off")
	}
	fmt.Fprintln(w, "include_path")
	if len(r.Include) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, d := range r.Include {
		if d.Exists {
			fmt.Fprintf(w, "  ✓ %s\n", short(d.Path))
		} else {
			fmt.Fprintf(w, "  ✗ %-28s directory does not exist\n", short(d.Path))
			status = 1
		}
	}
	fmt.Fprintln(w, "exclude_path")
	if len(r.Exclude) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, d := range r.Exclude {
		fmt.Fprintf(w, "  - %s\n", short(d))
	}
	if r.Effective == "" {
		fmt.Fprintln(w, "\nwarning: the search path is empty: no bare command name is run on the host")
	}
	fmt.Fprintln(w, "\nshims")
	if len(r.Shims) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	width := 0
	for _, s := range r.Shims {
		width = max(width, len(s.Name))
	}
	for _, s := range r.Shims {
		if s.Path != "" {
			fmt.Fprintf(w, "  ✓ %-*s → %s\n", width, s.Name, short(s.Path))
		} else {
			fmt.Fprintf(w, "  ✗ %-*s not in the search path; not created\n", width, s.Name)
			status = 1
		}
	}
	if sb != nil {
		fmt.Fprintf(w, "\nin this app (%s)\n", short(sb.Dir))
		want := r.Found()
		sort.Strings(want)
		have := append([]string(nil), sb.Names...)
		sort.Strings(have)
		switch {
		case sb.Missing && len(want) == 0:
			fmt.Fprintln(w, "  no shims, as configured")
		case sb.Missing:
			fmt.Fprintln(w, "  no shims yet")
		case len(have) == 0:
			fmt.Fprintln(w, "  (empty)")
		default:
			fmt.Fprintf(w, "  %s\n", strings.Join(have, " "))
		}
		if strings.Join(want, " ") != strings.Join(have, " ") {
			fmt.Fprintln(w, "  differs from the configuration: shims are made when the app starts; restart it")
		}
	}
	if r.UseLogin && len(r.Shims) > 0 {
		fmt.Fprintf(w, "\nShims are looked up with the login shell's PATH in %s, where the app started.\n", short(r.Cwd))
		fmt.Fprintln(w, "A tool only a project directory has (mise, direnv) gets no shim.")
	}
	return status
}
