// Package cli parses the command line of the two user-facing entry points:
//
//	flatpark-spawn [options] [cmd args...]   ours
//	host-spawn     [options] [cmd args...]   1player/host-spawn v1.6.2's,
//	                                         chosen by the basename
//	<any other name> [args...]               host-spawn's shim: a symlink
//	                                         named after a command runs it
//
// Both accept host-spawn's options (-pty, -no-pty, -cwd, -env, -version),
// with one or two dashes, up to the first non-option.
package cli

import (
	"bytes"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/flatpark/flatpark-spawn/internal/names"
)

// Kind of entry point.
type Kind int

const (
	Ours Kind = iota
	Compat
	Shim
)

// Options is a parsed command line.
type Options struct {
	Kind    Kind
	Pty     bool
	NoPty   bool
	Cwd     string
	Env     string // -env list; for Compat and Shim the only variables passed
	EnvSet  bool
	Version bool
	Help    bool
	Usage   string
	Args    []string
}

// Blocklist: programs that misbehave on a PTY (host-spawn #12).
var Blocklist = map[string]bool{
	"gio": true, "podman": true, "kde-open": true, "kde-open5": true, "xdg-open": true,
}

// KindOf tells the entry point from argv[0].
func KindOf(argv0 string) Kind {
	base := filepath.Base(argv0)
	switch {
	case strings.HasPrefix(base, names.CompatName):
		return Compat
	case base == names.Name:
		return Ours
	default:
		return Shim
	}
}

// Parse parses argv (including argv[0]).
func Parse(argv []string) (Options, error) {
	o := Options{Kind: KindOf(argv[0])}
	if o.Kind == Shim {
		o.Env = "TERM"
		o.Args = append([]string{filepath.Base(argv[0])}, argv[1:]...)
		return o, nil
	}
	fs := flag.NewFlagSet(filepath.Base(argv[0]), flag.ContinueOnError)
	var usage bytes.Buffer
	fs.SetOutput(&usage)
	fs.BoolVar(&o.Pty, "pty", false, "Force allocate a pseudo-terminal for the host process")
	fs.BoolVar(&o.NoPty, "no-pty", false, "Do not allocate a pseudo-terminal for the host process")
	fs.BoolVar(&o.Version, "version", false, "Show this program's version")
	envDefault, envHelp := "", "Comma separated list of environment variables to pass to the host process, in addition to the filtered environment."
	if o.Kind == Compat {
		envDefault, envHelp = "TERM", "Comma separated list of environment variables to pass to the host process."
	}
	fs.StringVar(&o.Env, "env", envDefault, envHelp)
	fs.StringVar(&o.Cwd, "cwd", "", "Change working directory of the spawned process")
	if err := fs.Parse(argv[1:]); err != nil {
		if err == flag.ErrHelp {
			o.Help = true
			fs.PrintDefaults()
			o.Usage = usageText(o.Kind, argv[0]) + usage.String()
			return o, nil
		}
		return o, fmt.Errorf("%s", strings.TrimSpace(usage.String()))
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "env" {
			o.EnvSet = true
		}
	})
	o.Args = fs.Args()
	return o, nil
}

func usageText(k Kind, argv0 string) string {
	what := "If COMMAND is not set, open the user's login shell on the host."
	if k == Compat {
		what = "If COMMAND is not set, spawn the user's default shell on the host."
	}
	return fmt.Sprintf(`Usage: %s [options] [ COMMAND [ arguments... ] ]

Run COMMAND on the host from inside a Flatpak sandbox (%s %s).
%s
A pseudo-terminal is allocated when stdin and stdout are terminals
(host-spawn: when stdout is a terminal), unless -pty or -no-pty says otherwise.
%s
Options:
`, filepath.Base(argv0), names.Name, names.Version, what, checkText(k))
}

func checkText(k Kind) string {
	if k != Ours {
		return ""
	}
	return fmt.Sprintf(`
%[1]s check   checks the host's configuration (~/.config/%[1]s/config.yaml)
                       and the shims; %[1]s -- check runs a host command "check".
`, names.Name)
}

// WantPty decides whether to allocate a PTY, given which of stdin and
// stdout are terminals. Ours wants both: a pipe on either side means a
// program reads or writes bytes, and a PTY would add \r and echo. The
// host-spawn entry points keep upstream's rule (stdout a terminal, the
// command not blocklisted).
func (o Options) WantPty(stdinTTY, stdoutTTY bool) bool {
	if o.Pty {
		return true
	}
	if o.NoPty {
		return false
	}
	if o.Kind == Ours {
		return stdinTTY && stdoutTTY
	}
	return stdoutTTY && (len(o.Args) == 0 || !Blocklist[o.Args[0]])
}

// DefaultCommand is what runs on the host when no command was given, or nil for
// ours (the agent opens the login shell).
func (o Options) DefaultCommand() []string {
	if o.Kind == Ours {
		return nil
	}
	return []string{"sh", "-c", "$SHELL"}
}
