# flatpark-spawn

flatpark-spawn lets a Flatpak app use the host's command-line tools as if
they were installed in its sandbox: system commands, `~/.local/bin`, and what
mise, nvm or cargo put in the user's login-shell PATH (`node`, `go`, `gh`,
...). The app itself is not changed. When it runs `git status`, the host's
git runs, with the user's configuration, credential helpers and PATH; its
exit code, output and signals reach the app as if it were a local child.

It is shipped as one Flatpak extension, `org.flatpark.Spawn`, shared by every
app that declares it, on any runtime (freedesktop, GNOME, KDE).

## Why this exists, and what it costs

A great many apps depend on toolchains from inside their sandbox: editors
and IDEs, and above all AI agent apps, which run `git`, `node`, `python`,
`gh`, language servers and whatever the user has installed. In a Flatpak
sandbox those tools are missing or are the wrong versions, and the app is
crippled.

**Be clear about what this is: flatpark-spawn takes the Flatpak sandbox
apart completely.** A command forwarded through it runs on the host as the
user, with everything the user can reach. Narrowing the app's other
permissions (no `--filesystem=home`, no `--filesystem=host`) does not
contain it.

We made it anyway, following FlatPark's principle that users trust FlatPark
because they trust the upstream app. If a user is willing to install an app
on the host with no restrictions at all, what they want from Flatpak is not
the sandbox but a better way to install and update it. On distributions
without the AUR, and especially on immutable ones, that matters a lot.

What we promise in return:

- **Every FlatPark app that uses this extension is audited as completely as
  we can.** For closed-source apps from well-known vendors, where we cannot
  audit the code, we guarantee only that we do not modify the original
  distribution.
- **flatpark-spawn itself is open source and auditable.** It may have bugs,
  and it may have vulnerabilities, but we will never do anything
  deliberately malicious in this layer.

Mechanically: the permission is granted by the app's manifest
(`--talk-name=org.freedesktop.Flatpak`), which FlatPark reviews, and the
user can take it away with
`flatpak override --user --no-talk-name=org.freedesktop.Flatpak <app-id>`.
Without that permission the extension does nothing at all.

## Using it in an app

A few lines in the manifest and one in the launcher. The extension is then
installed along with the app.

```yaml
finish-args:
  - --talk-name=org.freedesktop.Flatpak
add-extensions:
  org.flatpark.Spawn:
    directory: lib/flatpark-spawn
    version: '1'
    # no `no-autodownload`: it comes with the app
    # no `autodelete: true`: that would delete it under the other apps using it
modules:
  - name: app
    build-commands:
      - mkdir -p /app/lib/flatpark-spawn   # the mount point must exist
      # ...
```

In the launcher, before it execs the app:

```sh
[ -r /app/lib/flatpark-spawn/enable.sh ] && . /app/lib/flatpark-spawn/enable.sh
exec /app/extra/vendor/app "$@"
```

The line is harmless when the extension is not installed (the launcher starts
normally) and when the app lacks the permission (nothing is set). It works
under `set -eu`. For Electron apps on the Electron BaseApp, `enable.sh` also
sets `ZYPAK_LD_PRELOAD`, so the library reaches the processes zypak starts.

Some programs remove `LD_PRELOAD` before they start a child, and the library
is gone from that child and everything below it. VS Code (and Cursor,
Windsurf and other forks) does this for its extension host and pty host.
There, commands an extension runs, and same-name commands in the
integrated terminal (`git`), stay in the sandbox. Only bash's fallback for
commands the sandbox lacks still works. flatpark-spawn does not put the
library back (see [DESIGN.md](DESIGN.md), 否决的做法). For the commands that
matter, use [shims](#configuration-and-shims) instead.

A command the app must keep running from its own sandbox even though the host
has one of the same name goes in `FLATPARK_SPAWN_LOCAL`, set in the launcher:

```sh
export FLATPARK_SPAWN_LOCAL=python3:ssh
```

## What runs where

| The app runs | Where |
|---|---|
| a bare name the host has (`git`, `node`, `cat`) | the host, found in the login-shell PATH of the command's working directory (so a project's mise/asdf pins apply); also when the sandbox has the same name |
| a bare name only the sandbox has (`/app/bin/tool`) | the sandbox |
| a bare name nobody has | fails with ENOENT / 127, exactly as without flatpark-spawn |
| a command whose arguments or working directory are sandbox-only paths (`cat /app/x`, `sh -c 'cat /tmp/x'`) | the sandbox, if it has the command |
| a name in `FLATPARK_SPAWN_LOCAL` | the sandbox |
| an absolute path that exists in the sandbox (`/usr/bin/python3`) | the sandbox |
| an absolute path missing in the sandbox (`/usr/bin/nvim`, `/usr/bin/docker`, `~/.local/bin/x`) | the host, as given; missing there too: ENOENT message and exit 127 |
| an absolute path missing in the sandbox under a location the host does not have (`/app/x`, `/run/host/x`, a private `/tmp/x`) | the sandbox (ENOENT) |
| a shim (`nvim` with `shims: [nvim]`), from any process, with or without the library | by name, as above: the host, unless `FLATPARK_SPAWN_LOCAL` or sandbox-only arguments keep it here; the host lacking it, the sandbox's own copy |

Sandbox-only paths in arguments and the working directory: `/app`, `/usr`,
`/bin`, `/sbin`, `/lib*`, `/tmp` and `/var/tmp` (unless the app has
`--filesystem=/tmp`), `/run/flatpak`, `/run/host`, `/.flatpak-info`,
`/dev/fd`, `/proc/self/fd`. `/etc` is not on the list. For the program being
run, the list is shorter: `/usr`, `/bin`, `/sbin` and `/lib*` are left out,
since the host has those trees too and a file the runtime lacks there can
only be the host's. The reasoning, and how a command's host environment is
put together, are in [DESIGN.md](DESIGN.md).

## Configuration and shims

The host's `~/.config/flatpark-spawn/config.yaml` (`$XDG_CONFIG_HOME`), if it
exists, applies to every app that uses flatpark-spawn. Only the host side
reads it, so the app needs no access to your home directory. Without the
file everything works as described above.

```yaml
# Where to look for the commands apps run. First the login shell's PATH
# (what your terminal has, project pins from mise/asdf included), then
# include_path. exclude_path is skipped, also when the login PATH has it.
use_login_shell_path: true
include_path:
  - /opt/tools/bin
exclude_path:
  - /snap/bin

# Shims: a file per command in the app's PATH that runs it on the host.
# For programs that look for a command on disk before running it, and for
# processes without the preload library (VS Code's extension host).
shims:
  - nvim
  - rg
```

- `include_path`, `exclude_path`: absolute paths or `~/...`.
- A bare command name is run only if it is found in the search path: the
  login PATH (unless `use_login_shell_path: false`), then `include_path`,
  minus `exclude_path`. The command gets that search path as its `PATH`.
- An absolute path (`/usr/bin/nvim`) needs no `include_path`. Under an
  `exclude_path` directory it is treated as missing on the host.
- A changed file applies to running apps at their next lookup on the host.
  An answer already cached is used for up to 30 s.
- Shims are made when the app starts, only for the names found in the
  search path (looked up in the directory the app was started in). Restart
  the app after adding one. A name that is not found gets no shim, and the
  app says so on stderr when it starts. A shim also respects
  `FLATPARK_SPAWN_LOCAL` and the sandbox-only paths of [What runs
  where](#what-runs-where), like the library.
- A mistake in the file (an unknown key, a wrong type, a YAML error) makes
  flatpark-spawn ignore the whole file and print a warning. Commands then
  run as without the file. Find the problem with:

```
$ flatpark-spawn check
config: ~/.config/flatpark-spawn/config.yaml (found)

use_login_shell_path   ✓ 14 directories (login shell in ~)
include_path
  ✓ /opt/tools/bin
exclude_path
  - /snap/bin

shims
  ✓ nvim → /usr/bin/nvim
  ✗ rg   not in the search path; not created
```

Run it on the host, or in an app (`/app/lib/flatpark-spawn/bin/flatpark-spawn
check`), where it also shows the app's current shims. It exits 1 when
something is marked ✗ or the file has an error. `flatpark-spawn -- check`
runs a host command named `check`.

**vscode-neovim.** The extension runs in VS Code's extension host, which
has no preload library. It looks for `nvim` on disk before running it, so
neither the library nor bash's fallback can help. With

```yaml
shims:
  - nvim
```

it finds the shim and starts the host's nvim with its default settings.
Leave `vscode-neovim.neovimExecutablePaths.linux` unset (or set it to
`nvim`). An absolute path such as `/usr/bin/nvim` does not work there: the
extension host has no library to send it to the host.

To switch flatpark-spawn off for one app, remove its permission:

```sh
flatpak override --user --no-talk-name=org.freedesktop.Flatpak <app-id>
```

## The command

The extension also has the command itself, for apps and users that run host
commands explicitly:

```
/app/lib/flatpark-spawn/bin/flatpark-spawn [options] [command [args...]]
```

Without a command it opens the user's login shell on the host. A
pseudo-terminal is allocated when stdin and stdout are both terminals and
the command runs in the terminal's foreground, never for pipes. Options: `-pty`, `-no-pty`, `-cwd DIR`, `-env VAR,...` (variables
to pass in addition to the filtered environment), `-version`.

### host-spawn compatibility

Called as `host-spawn` (the extension ships `bin/host-spawn`; an app can link
`/app/bin/host-spawn` to `/app/lib/flatpark-spawn/bin/flatpark-spawn`) it
takes [host-spawn](https://github.com/1player/host-spawn)'s command line:
`-pty`, `-no-pty`, `-cwd`, `-env` (default `TERM`, and only the listed
variables are passed), `-version`, and no command opens `$SHELL`. A PTY is
allocated when stdout is a terminal, as upstream does. A symlink with any
other name runs that command on the host, as host-spawn's shims do. Existing
terminal configurations such as VS Code's `"path": "host-spawn"` keep working.

## Known limits

- Statically linked callers (Go programs, musl Rust such as Codex) are not
  seen by `LD_PRELOAD`; what they run through `sh -c` or another dynamic
  program still is.
- Shells other than bash that search PATH themselves (zsh, fish inside the
  sandbox) do not forward a missing command; bash, `sh` (bash in the
  freedesktop runtimes) and anything that calls `exec` do.
- The host cannot see the sandbox: a script in `/app`, an argument naming an
  `/app` file, or a callback such as `GIT_ASKPASS=/app/...` does not work on
  the host.
- `fexecve`/`execveat` are not forwarded.
- A process whose parent removed `LD_PRELOAD` has no library: VS Code's
  extension host and pty host and everything they start. There only shims
  (`shims:` in config.yaml) and bash's fallback for commands the sandbox
  lacks reach the host; a same-name command (`git`) runs in the sandbox.
- Shims are made when the app starts: a newly configured shim appears
  after a restart, and a tool only a project directory has (mise, direnv)
  gets none unless the app was started there. Each app start costs one
  more trip to the host (about 10 ms) to make them.
- An absolute path missing in the sandbox and on the host alike
  (`/usr/bin/nope`) has already been handed to the host when that turns out:
  the caller sees `No such file or directory` and exit 127 (a shell reports
  127 as it would anyway), but Python's `subprocess` gets returncode 127
  rather than `FileNotFoundError`. A bare name missing everywhere still
  fails exactly as without flatpark-spawn.
- From a terminal, a forwarded command gets a terminal of its own on the
  host (so `/dev/tty`, sudo and ssh prompts, editors, fzf and less work),
  and Ctrl-C interrupts it. Ctrl-Z typed on the terminal does nothing; a
  stop sent by the calling shell (`kill -TSTP`) works. Keys typed ahead
  while such a command runs go to it, and are lost if it does not read
  them. A background job of the terminal gets no terminal of its own (as
  a native background job, it should not use one).
- A forwarded command costs about 5 ms (a D-Bus round trip through the
  Flatpak session helper); a local one about 50 µs more than without
  flatpark-spawn. The first command in a directory not seen before costs
  one run of the user's login shell more (about 75 ms here), since the
  login PATH is taken per directory.
- After `mise use` switches a tool's version, the host may run the old one
  for up to about a minute and a half (cached login PATH).

## Building and testing

```
make            # lint, unit tests, build
make test       # Go unit tests, including the C routing rules through cgo
make payload    # .build/dist/flatpark-spawn-<version>-<arch>.tar.gz
make e2e        # see below
```

`make e2e` builds the payload, the extension and two test apps
(freedesktop 26.08 and GNOME 51), installs them from a temporary local
remote with the payload served by a local HTTP server, runs the matrix and
the timings in both, and removes everything it added. It builds under ids
of its own (`org.flatpark.SpawnE2E*`, remote `flatpark-spawn-e2e-run`), so
an installed `org.flatpark.Spawn` and the apps using it are not touched. It needs flatpak,
`org.flatpak.Builder`, the two runtimes with their SDKs, python3 and
`script(1)`, and installs the Electron BaseApp for the run if it is missing.
The agent reads a config.yaml of the run's own
(`FLATPARK_SPAWN_CONFIG`, set in the test apps), never
`~/.config/flatpark-spawn`. `E2E_KEEP=1 make e2e` leaves the test setup installed;
`tests/e2e/run.sh --clean` removes it.

Names (`flatpark-spawn`, `FLATPARK_SPAWN_*`, the mount point, ...) are
defined once, in `names.mk`; `make gen` regenerates `internal/names`.

Releases: pushing a `v*` tag builds the x86_64 and aarch64 tarballs and
publishes them with their sha256 and sizes, which
`packaging/org.flatpark.Spawn.yml` pins as extra-data.

## Credits

The pseudo-terminal code (`internal/pty`), the host-spawn command line and
the approach of running commands through the Flatpak session helper's
`HostCommand` come from [1player/host-spawn](https://github.com/1player/host-spawn)
by Stéphane Travostino, under the MIT-0 license. Thank you.

## License

MIT, see [LICENSE](LICENSE).
