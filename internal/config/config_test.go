package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const home = "/home/me"

func parse(t *testing.T, body string) (Config, *Error) {
	t.Helper()
	c, err := Parse([]byte(body), "config.yaml", home)
	if err == nil {
		return c, nil
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("not an *Error: %v", err)
	}
	if !reflect.DeepEqual(c, Default()) {
		t.Fatalf("an error must give the defaults, got %+v", c)
	}
	return c, ce
}

func TestDefaults(t *testing.T) {
	for _, body := range []string{"", "# only a comment\n", "---\n"} {
		c, e := parse(t, body)
		if e != nil || !reflect.DeepEqual(c, Default()) || !c.UseLoginShellPath {
			t.Errorf("%q: %+v %v", body, c, e)
		}
	}
	c, exists, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), home)
	if err != nil || exists || !reflect.DeepEqual(c, Default()) {
		t.Fatalf("missing file: %+v %v %v", c, exists, err)
	}
}

func TestEveryKey(t *testing.T) {
	c, e := parse(t, `# a comment
use_login_shell_path: false
include_path:
  - /opt/tools/bin
  - ~/bin/
  - /opt/tools//bin/../bin
exclude_path: [/snap/bin, "~"]
shims:
  - nvim
  - rg
  - nvim
`)
	if e != nil {
		t.Fatal(e)
	}
	want := Config{
		UseLoginShellPath: false,
		IncludePath:       []string{"/opt/tools/bin", home + "/bin", "/opt/tools/bin"},
		ExcludePath:       []string{"/snap/bin", home},
		Shims:             []string{"nvim", "rg"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
	if c, e := parse(t, "use_login_shell_path: yes\n"); e == nil || c.UseLoginShellPath != true {
		t.Errorf("yes is not a YAML 1.2 boolean here: %+v %v", c, e)
	}
	if c, e := parse(t, "include_path:\nshims: []\n"); e != nil || len(c.IncludePath) != 0 {
		t.Errorf("empty lists: %+v %v", c, e)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"include_paths: [/x]\n", `config.yaml:1: unknown key "include_paths" (did you mean include_path?)`},
		{"a: 1\nfrobnicate: 2\n", `config.yaml:1: unknown key "a" (known: use_login_shell_path, include_path, exclude_path, shims); config.yaml:2: unknown key "frobnicate"`},
		{"use_login_shell_path: maybe\n", "config.yaml:1: use_login_shell_path must be true or false"},
		{"use_login_shell_path:\n", "config.yaml:1: use_login_shell_path must be true or false"},
		{"include_path: /opt/bin\n", "config.yaml:1: include_path must be a list"},
		{"include_path:\n  - bin\n", `config.yaml:2: include_path: "bin" is not an absolute path (start with / or ~/)`},
		{"exclude_path:\n  - ~other/bin\n", `config.yaml:2: exclude_path: "~other/bin" is not supported`},
		{"include_path:\n  - [a]\n", "config.yaml:2: include_path: each item must be a single string"},
		{"shims: [nvim, a/b, ..]\n", `config.yaml:1: shims: "a/b" is not a command name`},
		{"shims:\n  - true\n", `config.yaml:2: shims: "true" is not a string (quote it if you mean it)`},
		{"shims: [x]\nshims: [y]\n", `config.yaml:2: "shims" is set twice (first on line 1)`},
		{"- nvim\n", "config.yaml:1: the file must be a mapping of keys"},
		{"shims: [nvim\n", "config.yaml:"},
		// yaml.v3's line: where the construct it could not finish starts.
		{"include_path:\n  - /a\n bad: [\n", "config.yaml:2: YAML syntax: did not find expected key"},
		{"shims: [a]\n---\nshims: [b]\n", "config.yaml:2: more than one YAML document"},
	} {
		_, e := parse(t, tc.body)
		if e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("%q:\n got  %v\n want %s", tc.body, e, tc.want)
		}
	}
	// Problems come by line, and each has one.
	_, e := parse(t, "shims: [a/b]\ninclude_path: [rel]\n")
	if e == nil || len(e.Problems) != 2 || e.Problems[0].Line != 1 || e.Problems[1].Line != 2 {
		t.Fatalf("%+v", e)
	}
	if _, e := Parse([]byte("include_path: [~/x]\n"), "c", ""); e == nil || !strings.Contains(e.Error(), "HOME is not set") {
		t.Errorf("~ without HOME: %v", e)
	}
}

func TestPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"HOME": "/h"}, "/h/.config/flatpark-spawn/config.yaml"},
		{map[string]string{"HOME": "/h", "XDG_CONFIG_HOME": "/x"}, "/x/flatpark-spawn/config.yaml"},
		{map[string]string{"HOME": "/h", "XDG_CONFIG_HOME": "rel"}, "/h/.config/flatpark-spawn/config.yaml"},
		{map[string]string{"HOME": "/h", "FLATPARK_SPAWN_CONFIG": "/t/c.yaml"}, "/t/c.yaml"},
		{map[string]string{"HOME": "/h", "FLATPARK_SPAWN_CONFIG": "c.yaml"}, "/h/.config/flatpark-spawn/config.yaml"},
	} {
		if got := Path(env(tc.env)); got != tc.want {
			t.Errorf("%v: %s, want %s", tc.env, got, tc.want)
		}
	}
}

func TestEffective(t *testing.T) {
	login := "/home/me/.local/bin:/usr/bin::/snap/bin:/usr/bin/:rel"
	for _, tc := range []struct {
		name string
		c    Config
		want string
	}{
		{"nothing configured: unchanged", Default(), login},
		{"include appended, duplicates keep the first place",
			Config{UseLoginShellPath: true, IncludePath: []string{"/opt/b", "/usr/bin"}},
			"/home/me/.local/bin:/usr/bin::/snap/bin:rel:/opt/b"},
		{"exclude applies to the login PATH",
			Config{UseLoginShellPath: true, ExcludePath: []string{"/snap/bin", "/usr/bin"}},
			"/home/me/.local/bin::rel"},
		{"exclude applies to include_path",
			Config{UseLoginShellPath: true, IncludePath: []string{"/opt/b"}, ExcludePath: []string{"/opt/b"}},
			"/home/me/.local/bin:/usr/bin::/snap/bin:rel"},
		{"login PATH off", Config{IncludePath: []string{"/opt/b", "/opt/c"}}, "/opt/b:/opt/c"},
		{"empty", Config{}, ""},
	} {
		if got := tc.c.Effective(login); got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

func TestExcluded(t *testing.T) {
	c := Config{ExcludePath: []string{"/snap/bin", "/opt"}}
	for p, want := range map[string]bool{
		"/snap/bin/x":       true,
		"/snap/bin//x":      true,
		"/opt/tools/bin/x":  true,
		"/snap/binary/x":    false,
		"/snap/x":           false,
		"/usr/bin/x":        false,
		"relative/snap/bin": false,
	} {
		if got := c.Excluded(p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestFingerprint(t *testing.T) {
	f := filepath.Join(t.TempDir(), "config.yaml")
	if Fingerprint(f) != "" {
		t.Fatal("no file, no fingerprint")
	}
	if err := os.WriteFile(f, []byte("shims: [a]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Fingerprint(f)
	if err := os.WriteFile(f, []byte("shims: [ab]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f, when, when); err != nil {
		t.Fatal(err)
	}
	b := Fingerprint(f)
	if err := os.WriteFile(f, []byte("shims: [ac]\n"), 0o644); err != nil { // same size
		t.Fatal(err)
	}
	if err := os.Chtimes(f, when.Add(time.Second), when.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	c := Fingerprint(f)
	if a == "" || a == b || b == c {
		t.Fatalf("size and mtime both count: %s %s %s", a, b, c)
	}
}

func TestRender(t *testing.T) {
	ok := Report{File: "/home/me/.config/flatpark-spawn/config.yaml", Exists: true, UseLogin: true, LoginDirs: 3,
		Include: []Dir{{"/opt/b", true}}, Exclude: []string{"/snap/bin"}, Effective: "/a",
		Shims: []Shim{{Name: "nvim", Path: "/usr/bin/nvim"}}, Cwd: "/home/me"}
	var out bytes.Buffer
	if st := Render(&out, ok, home, &Sandbox{Dir: "/run/x/shims", Names: []string{"nvim"}}); st != 0 {
		t.Fatalf("status %d\n%s", st, out.String())
	}
	for _, want := range []string{"config: ~/.config/flatpark-spawn/config.yaml (found)", "✓ 3 directories", "✓ /opt/b",
		"- /snap/bin", "✓ nvim → /usr/bin/nvim", "in this app (/run/x/shims)\n  nvim\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "restart") {
		t.Errorf("in step with the configuration:\n%s", out.String())
	}

	bad := ok
	bad.Include = []Dir{{"/opt/old", false}}
	bad.Shims = append(bad.Shims, Shim{Name: "rg", Checked: []string{"/a"}})
	out.Reset()
	if st := Render(&out, bad, home, &Sandbox{Dir: "/run/x/shims", Missing: true}); st != 1 {
		t.Fatalf("status %d", st)
	}
	for _, want := range []string{"✗ /opt/old", "directory does not exist", "✗ rg", "not created", "no shims yet", "restart it"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in\n%s", want, out.String())
		}
	}

	out.Reset()
	empty := Report{File: "/c", Exists: true}
	if st := Render(&out, empty, home, nil); st != 0 || !strings.Contains(out.String(), "warning: the search path is empty") {
		t.Fatalf("empty search path is a warning: %d\n%s", st, out.String())
	}
	out.Reset()
	broken := Report{File: "/c", Exists: true, Errors: []string{"/c:7: unknown key \"x\""}}
	if st := Render(&out, broken, home, nil); st != 1 || !strings.Contains(out.String(), "✗ /c:7: unknown key") {
		t.Fatalf("errors: %d\n%s", st, out.String())
	}
}
