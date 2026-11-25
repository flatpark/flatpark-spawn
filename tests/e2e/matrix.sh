#!/bin/sh
# The e2e matrix for one installed consumer app (run.sh sets it up).
# Usage: WORK=dir matrix.sh APP-ID. Prints ok/FAIL/skip per check.
set -u

APP=$1
T=$WORK/matrix-$APP
mkdir -p "$T"
P=/app/libexec/spawn-test/exec-probe
SPAWN=/app/lib/flatpark-spawn/bin/flatpark-spawn
pass=0
failed=0
skipped=0

ok() { pass=$((pass + 1)); echo "ok   - $1"; }
bad() {
    failed=$((failed + 1))
    echo "FAIL - $1"
    shift
    for l in "$@"; do printf '       %s\n' "$l"; done
}
skip() { skipped=$((skipped + 1)); echo "skip - $1: $2"; }
run() { flatpak run "$APP" "$@"; }

# expect NAME WANT CMD...: stdout must be exactly WANT, exit 0.
expect() {
    name=$1 want=$2
    shift 2
    got=$(run "$@" 2>"$T/err")
    rc=$?
    if [ "$rc" = 0 ] && [ "$got" = "$want" ]; then
        ok "$name"
    else
        bad "$name" "want: $want" "got:  $got (exit $rc)" "stderr: $(head -c 300 "$T/err")"
    fi
}

CFG=${CFG:?run.sh sets CFG}
INC=${INC:?run.sh sets INC}

# expect_rc NAME RC CMD...: exit code only.
expect_rc() {
    name=$1 want=$2
    shift 2
    run "$@" >"$T/out" 2>"$T/err"
    rc=$?
    if [ "$rc" = "$want" ]; then
        ok "$name"
    else
        bad "$name" "want exit $want, got $rc" "stdout: $(head -c 200 "$T/out")" "stderr: $(head -c 300 "$T/err")"
    fi
}

host_os=$(cat /etc/os-release)
login() { "${SHELL:-/bin/sh}" -lic "$1" </dev/null 2>/dev/null | tail -n1; }

# ---- enabled ----
expect "enable.sh probed and set the preload" \
    "1 /app/lib/flatpark-spawn/lib/libflatpark-spawn.so" \
    sh -c 'echo "$FLATPARK_SPAWN_ENABLE $LD_PRELOAD"'

# ---- tools only in the login PATH ----
tool=
for t in node go gh; do
    if [ -n "$(login "command -v $t")" ] && ! env -i PATH="$(systemctl --user show-environment 2>/dev/null | sed -n 's/^PATH=//p')" sh -c "command -v $t" >/dev/null 2>&1; then
        tool=$t
        break
    fi
done
if [ -n "$tool" ]; then
    flag=--version
    [ "$tool" = go ] && flag=version
    want=$(login "$tool $flag")
    expect "login-PATH-only tool: execvp $tool $flag matches the host's \$SHELL -lic" "$want" $P execvp "$tool" "$flag"
    expect "login-PATH-only tool: Python subprocess $tool $flag" "$want" \
        python3 -c "import subprocess; subprocess.run(['$tool', '$flag'], check=True)"
else
    skip "login-PATH-only tool" "none of node, go, gh is in the login PATH but not the session PATH here"
fi

# ---- the login PATH follows the working directory (mise) ----
# A temporary project with a mise.toml naming a tool that is already
# installed: mise then puts that tool first in PATH there, nothing gets
# installed and the user's mise configuration is not touched. mise does
# record the config under ~/.local/state/mise/tracked-configs; those links
# (and only those) are removed again.
session_path=$(systemctl --user show-environment 2>/dev/null | sed -n 's/^PATH=//p')
host_login_path() { # dir: the login PATH there, from the session environment as the agent sees it
    (cd "$1" && env -i HOME="$HOME" USER="${USER-}" LOGNAME="${USER-}" SHELL="${SHELL:-/bin/sh}" \
        PATH="${session_path:-/usr/bin:/bin}" XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR-}" TERM=dumb \
        "${SHELL:-/bin/sh}" -lic 'printf "\n__P=%s\n" "$PATH"' </dev/null 2>/dev/null | sed -n 's/^__P=//p' | tail -n1)
}
untrack_mise() { # dir
    for l in "$HOME"/.local/state/mise/tracked-configs/*; do
        case "$(readlink "$l" 2>/dev/null)" in "$1"/*) rm -f "$l" ;; esac
    done
}
mise_tool=
if command -v mise >/dev/null 2>&1; then
    for t in go node; do
        if mise where "$t" >/dev/null 2>&1; then mise_tool=$t; break; fi
    done
fi
if [ -n "$mise_tool" ]; then
    proj=$(mktemp -d "$HOME/.cache/fps-test-mise.XXXXXX")
    printf '[tools]\n%s = "latest"\n' "$mise_tool" >"$proj/mise.toml"
    want_proj=$(host_login_path "$proj")
    want_home=$(host_login_path "$HOME")
    print_path='printf "%s" "$PATH"'
    got_proj=$(flatpak run --filesystem="$proj" --cwd="$proj" "$APP" $P execvp sh -c "$print_path" 2>"$T/err")
    got_home=$(flatpak run --cwd="$HOME" "$APP" $P execvp sh -c "$print_path" 2>>"$T/err")
    if [ "${want_proj%%:*}" = "${want_home%%:*}" ]; then
        skip "login PATH per directory" "a mise.toml with $mise_tool did not change the PATH order here"
    elif [ "${got_proj%%:*}" = "${want_proj%%:*}" ] && [ "${got_home%%:*}" = "${want_home%%:*}" ]; then
        ok "login PATH per directory: in a mise project $mise_tool comes first, as in the host's shell there (${got_proj%%:*})"
    else
        bad "login PATH per directory" "project: want ${want_proj%%:*}, got ${got_proj%%:*}" \
            "home:    want ${want_home%%:*}, got ${got_home%%:*}" "stderr: $(head -c 300 "$T/err")"
    fi
    untrack_mise "$proj"
    rm -rf "$proj"
else
    skip "login PATH per directory" "no mise with go or node installed on the host"
fi

# ---- same name: host by default, sandbox for sandbox paths ----
expect "same name goes to the host: execvp cat /etc/os-release" "$host_os" $P execvp cat /etc/os-release
expect "same name goes to the host: Python subprocess cat" "$host_os" \
    python3 -c "import subprocess; subprocess.run(['cat', '/etc/os-release'], check=True)"
expect "same name goes to the host: bash's own PATH search" "$host_os" /bin/sh -c 'cat /etc/os-release'
expect "/app argument stays in the sandbox: cat /app/..." sandbox-marker $P execvp cat /app/share/spawn-test/marker.txt
expect "/app inside sh -c stays in the sandbox" sandbox-marker $P execvp sh -c 'cat /app/share/spawn-test/marker.txt'
expect "sandbox-only tool runs in the sandbox" own-tool-ok $P execvp fps-own-tool
expect "FLATPARK_SPAWN_LOCAL keeps a name local" "$(flatpak run --command=cat "$APP" /etc/os-release)" \
    sh -c 'FLATPARK_SPAWN_LOCAL=sed:cat exec cat /etc/os-release'

# ---- indirect callers ----
expect "execvp of a host-only command" host-only-ok $P execvp fps-test-hostonly
expect "execlp" host-only-ok $P execlp fps-test-hostonly
expect "posix_spawnp" host-only-ok $P spawnp fps-test-hostonly
expect "absolute host path (no grant)" host-only-ok $P execve "$HOME/.local/bin/fps-test-hostonly"

# ---- an absolute /usr/bin path the sandbox lacks: the host's file ----
# A command the host really has in /usr/bin and the runtime does not; nothing
# is written to the host's /usr/bin. The first of the candidates the host has
# and this app lacks (checked with --command, i.e. without the preload) is
# used; on the machine this was written on that is nvim (flatpak, git,
# systemctl, journalctl also qualify there; neither freedesktop 26.08 nor
# GNOME 51 has any of them). Skipped when there is none.
usrcmd=
for c in nvim flatpak git systemctl journalctl podman dpkg rpm pacman; do
    [ -f "/usr/bin/$c" ] && [ -x "/usr/bin/$c" ] || continue
    flatpak run --command=sh "$APP" -c "[ ! -e /usr/bin/$c ]" 2>/dev/null || continue
    usrcmd=$c
    break
done
if [ -n "$usrcmd" ]; then
    want=$("/usr/bin/$usrcmd" --version 2>/dev/null)
    expect "/usr/bin/$usrcmd, missing in the sandbox: execve (preload) runs the host's" "$want" \
        $P execve "/usr/bin/$usrcmd" --version
    expect "/usr/bin/$usrcmd, missing in the sandbox: /bin/sh -c '/usr/bin/$usrcmd --version'" "$want" \
        /bin/sh -c "/usr/bin/$usrcmd --version"
    expect "/usr/bin/$usrcmd, missing in the sandbox: Python subprocess" "$want" \
        /usr/bin/python3 -c "import subprocess; subprocess.run(['/usr/bin/$usrcmd', '--version'], check=True)"
else
    skip "absolute /usr/bin path missing in the sandbox" \
        "none of nvim flatpak git systemctl journalctl podman dpkg rpm pacman is in the host's /usr/bin and missing in $APP"
fi
# Missing on both sides: the same as a missing ~/.local/bin path has always
# been. The process already is flatpark-spawn when the host says no, so the
# caller sees exit 127 and "No such file or directory" (a shell prints its
# own line as well); Python gets returncode 127 rather than
# FileNotFoundError (see DESIGN.md, 判定表).
rnd=$$-$(od -An -N4 -tx4 /dev/urandom | tr -d ' ')
for nope in "/usr/bin/fps-test-nope-$rnd" "$HOME/.local/bin/fps-test-nope-$rnd"; do
    run $P execve "$nope" >"$T/out" 2>"$T/err"
    rc=$?
    sh_out=$(run /bin/sh -c "$nope; echo rc=\$?" 2>"$T/err-sh")
    py_out=$(run /usr/bin/python3 -c "
import subprocess
try:
    print('rc=%d' % subprocess.run(['$nope'], stderr=subprocess.DEVNULL).returncode)
except FileNotFoundError:
    print('FileNotFoundError')" 2>/dev/null)
    if [ "$rc" = 127 ] && grep -q "$nope: No such file or directory" "$T/err" && [ ! -s "$T/out" ] &&
        [ "$sh_out" = rc=127 ] && grep -q "$nope: No such file or directory" "$T/err-sh" &&
        [ "$py_out" = rc=127 ]; then
        ok "missing on both sides, ${nope%%-$rnd}-*: execve 127 + ENOENT message, shell rc=127, Python rc=127"
    else
        bad "missing on both sides, $nope" "execve: exit $rc, stderr: $(head -c 200 "$T/err")" \
            "shell: $sh_out, stderr: $(head -c 200 "$T/err-sh")" "python: $py_out"
    fi
done
expect "sh -c (sh itself goes to the host)" host-only-ok $P execvp sh -c fps-test-hostonly
expect "/bin/sh -c (command_not_found_handle)" host-only-ok $P execve /bin/sh -c fps-test-hostonly
expect "nested /bin/sh -c" host-only-ok $P execve /bin/sh -c '/bin/sh -c fps-test-hostonly'
expect "bash -c" host-only-ok $P execve /bin/bash -c fps-test-hostonly
expect "an app's sh -c (launcher not preloaded, sh is)" host-only-ok sh -c fps-test-hostonly
expect "Python subprocess" host-only-ok python3 -c "import subprocess; subprocess.run(['fps-test-hostonly'], check=True)"
expect "Python subprocess shell=True" host-only-ok python3 -c "import subprocess; subprocess.run('fps-test-hostonly', shell=True, check=True)"
expect "Python os.system" host-only-ok python3 -c "import os, sys; sys.exit(os.waitstatus_to_exitcode(os.system('fps-test-hostonly')))"
expect "env (sandbox's /usr/bin/env)" host-only-ok $P execve /usr/bin/env fps-test-hostonly
expect "env (bare, goes to the host)" host-only-ok $P execvp env fps-test-hostonly
expect "xargs" host-only-ok sh -c 'echo | /usr/bin/xargs fps-test-hostonly'
expect "host-spawn shim symlink" host-only-ok /app/libexec/spawn-test/shim/fps-test-hostonly
node=$(login 'command -v node')
if [ -n "$node" ] && [ -x "$node" ]; then
    nodedir=$(dirname "$(dirname "$(readlink -f "$node")")")
    got=$(flatpak run --filesystem="$nodedir:ro" "$APP" "$(readlink -f "$node")" /app/share/spawn-test/node-spawn.js fps-test-hostonly 2>"$T/err")
    if [ "$got" = host-only-ok ]; then ok "Node child_process"; else bad "Node child_process" "got: $got" "$(cat "$T/err")"; fi
else
    skip "Node child_process" "no node on the host to run in the sandbox"
fi

# ---- a host command created now ----
new=fps-test-new-$$
printf '#!/bin/sh\necho new-ok\n' >"$HOME/.local/bin/$new"
chmod 755 "$HOME/.local/bin/$new"
expect "host command created during the test, called at once" new-ok $P execvp "$new"
late=fps-test-late-$$
run $P spawnp "$late" >/dev/null 2>&1 # a miss: cached as negative
printf '#!/bin/sh\necho late-ok\n' >"$HOME/.local/bin/$late"
chmod 755 "$HOME/.local/bin/$late"
sleep 4 # FPS_NEG_TTL_MISSING (3 s) after the miss
expect "host command created after a miss, available after the short negative TTL" late-ok $P execvp "$late"

# ---- exit codes, signals, output ----
expect_rc "exit code passes through (sh -c 'exit 42' on the host)" 42 $P spawnp sh -c 'exit 42'
expect_rc "a command's own exit 127 is its exit status" 127 $P spawnp fps-test-exit127
run $P spawnp fps-test-missing-$$ >"$T/out" 2>"$T/err"
rc=$?
if [ "$rc" = 111 ] && grep -q 'No such file or directory' "$T/err" && [ ! -s "$T/out" ]; then
    ok "really missing: posix_spawnp fails with ENOENT, as without flatpark-spawn"
else
    bad "really missing: ENOENT" "exit $rc" "$(cat "$T/err")"
fi
expect "Python tells FileNotFoundError from exit 127" "missing=FileNotFoundError own=127" python3 -c "
import subprocess
try:
    subprocess.run(['fps-test-missing-$$'])
    m = 'started'
except FileNotFoundError:
    m = 'FileNotFoundError'
r = subprocess.run(['fps-test-exit127'], capture_output=True).returncode
print(f'missing={m} own={r}')"
got=$(run /bin/sh -c "fps-test-missing-$$; echo rc=\$?" 2>"$T/err")
if [ "$got" = rc=127 ] && grep -q "fps-test-missing-$$: command not found" "$T/err"; then
    ok "shell: missing command is 127 + 'command not found'"
else
    bad "shell: missing command" "got: $got" "stderr: $(cat "$T/err")"
fi
expect "SIGTERM: the caller sees its child killed by SIGTERM (-15)" "rc=-15" python3 -c "
import subprocess, time
p = subprocess.Popen(['fps-test-sleep'], stdout=subprocess.PIPE)
time.sleep(1.5)
p.terminate()
print(f'rc={p.wait()}')"
sleep 0.5
# On the host (inside, pgrep would also see the sandbox-side helper).
if pgrep -f "$HOME/.local/bin/fps-test-sleep" >/dev/null || pgrep -x -f 'sleep 30' >/dev/null; then
    bad "SIGTERM: the host process group is gone" "$(pgrep -a -f 'fps-test-sleep|sleep 30')"
else
    ok "SIGTERM: the host process group is gone (sh and its sleep child)"
fi
expect "pipe output has no \\r and no NUL" "b'host-only-ok\\n'" python3 -c "
import subprocess; print(subprocess.run(['fps-test-hostonly'], capture_output=True).stdout)"
run $SPAWN printf 'a\nb\n' >"$T/raw"
if [ "$(od -An -c "$T/raw" | tr -s ' ')" = " a \n b \n" ]; then
    ok "flatpark-spawn in a pipe: no PTY, bytes unchanged"
else
    bad "flatpark-spawn in a pipe" "$(od -c "$T/raw")"
fi

# ---- terminal helpers ----
# on_tty INPUT-GENERATOR COMMAND: runs COMMAND (sh syntax) on a terminal
# provided by script(1) and prints what appeared on it, \r removed.
# - Its input is a FIFO that INPUT-GENERATOR writes to and that then stays
#   open until script is done: no extra input, in particular no EOF
#   character. (With `sleep N | script`, script types the terminal's EOF
#   character when its stdin ends; that byte woke up a relay blocked reading
#   the terminal and so, depending on timing, hid a terminal-restore hang
#   that waited for the next key press; see DESIGN.md, 前台判断.)
# - At most 30 s: then script is killed, the test app's instances too (a
#   hung flatpark-spawn would otherwise stay), "TIMEOUT after 30s" is printed
#   and the check fails on it instead of the whole e2e hanging.
if command -v script >/dev/null; then
    on_tty() {
        input="$T/tty-input.$$"
        rm -f "$input"
        mkfifo "$input"
        ( eval "$1"; exec sleep 40 ) >"$input" & feeder=$!
        { timeout -k 2s 30s script -qec "$2" /dev/null <"$input"; echo $? >"$T/tty-rc"; } | tr -d '\r'
        kill "$feeder" 2>/dev/null
        wait "$feeder" 2>/dev/null
        rm -f "$input"
        case $(cat "$T/tty-rc") in
        124|137)
            echo "TIMEOUT after 30s"
            flatpak kill "$APP" >/dev/null 2>&1
            ;;
        esac
    }
fi

# ---- PTY ----
if command -v script >/dev/null; then
    tty_id() { on_tty ':' "stty rows 33 cols 77; $1" | tail -n1; }
    hostdev=$(tty_id "stat -L -c '%d %t %T' /proc/self/fd/0")
    ptydev=$(tty_id "flatpak run $APP $SPAWN stat -L -c '%d %t %T' /proc/self/fd/0")
    nopty=$(tty_id "flatpak run $APP $SPAWN -no-pty stat -L -c '%d %t %T' /proc/self/fd/0")
    size=$(tty_id "flatpak run $APP $SPAWN stty size")
    if [ -n "$ptydev" ] && [ "$ptydev" != "$hostdev" ] && [ "$nopty" = "$hostdev" ] && [ "$size" = "33 77" ]; then
        ok "TTY: flatpark-spawn gives the host process a PTY of its own, with our size; -no-pty passes ours"
    else
        bad "TTY/PTY" "terminal: $hostdev" "default: $ptydev" "-no-pty: $nopty" "stty size: $size"
    fi
    size=$(tty_id "flatpak run $APP host-spawn stty size")
    if [ "$size" = "33 77" ]; then ok "TTY: host-spawn allocates a PTY like upstream"; else bad "host-spawn PTY" "stty size: $size"; fi
else
    skip "TTY" "no script(1) on the host"
fi

# ---- forwarding from a terminal: the host process gets one of its own ----
# Through the forwarding paths (preload exec, command_not_found_handle, the
# direct start from the cache), not the CLI. script(1) provides the
# terminal, and `flatpak run` is exec'd until it leads the session: flatpak
# moves itself into a process group of its own when it can, which without a
# job-control shell making that group the foreground one is a background
# job of the terminal. CMD runs in the app's sh (bash): nothing is quoted
# twice.
if command -v script >/dev/null; then
    in_app_tty() { # input-generator cmd: the terminal's output, lines joined
        printf 'stty rows 33 cols 77\nexec flatpak run %s sh -c %s\n' \
            "$APP" "'$2; echo rc=\$?'" >"$T/tty.sh"
        on_tty "$1" "exec sh $T/tty.sh" | tr '\n' ' ' | sed 's/  */ /g; s/ $//'
    }
    want="ctty=yes devtty=ok size=33 77 stdin=tty rc=7"
    # The caller has put the terminal in raw mode itself (fzf, an editor
    # running a command): set up and restored without blocking.
    got=$(in_app_tty ':' "/bin/stty raw -echo min 1; $P execvp fps-test-ttycheck")
    if [ "$got" = "$want" ]; then
        ok "terminal, caller already raw: controlling tty, /dev/tty, size, exit 7, no hang"
    else
        bad "terminal, caller already raw" "want: $want" "got:  $got"
    fi
    for how in "preload exec|$P execvp fps-test-ttycheck" \
               "preload exec again (direct start from the cache)|$P execvp fps-test-ttycheck" \
               "command_not_found_handle|/bin/sh -c fps-test-ttycheck"; do
        label=${how%%|*}
        got=$(in_app_tty ':' "${how#*|}")
        if [ "$got" = "$want" ]; then
            ok "terminal, $label: controlling tty, /dev/tty, size 33x77, exit 7"
        else
            bad "terminal, $label" "want: $want" "got:  $got"
        fi
    done
    want="ctty=yes devtty=ok size=33 77 stdin=piped rc=7"
    got=$(in_app_tty ':' 'printf piped | fps-test-ttycheck')
    if [ "$got" = "$want" ]; then
        ok "terminal, stdin a pipe: the pipe's data and a controlling tty (set by the agent)"
    else
        bad "terminal, stdin a pipe" "want: $want" "got:  $got"
    fi
    # A background job of the terminal must not touch it: no PTY of ours,
    # not stopped, exit code passed through.
    want="ctty=no devtty=fail size= stdin= rc=7"
    got=$(in_app_tty ':' "set -m; $P execvp fps-test-ttycheck </dev/null & wait \$!")
    if [ "$got" = "$want" ]; then
        ok "terminal, background job: left alone, not stopped, exit 7"
    else
        bad "terminal, background job" "want: $want" "got:  $got"
    fi
    # Ctrl-C typed on the terminal interrupts the host process.
    start=$(date +%s)
    got=$(in_app_tty "{ sleep 2; printf '\\003'; sleep 3; }" "$P execvp fps-test-wait" | grep -o 'rc=[0-9]*')
    took=$(($(date +%s) - start))
    if [ "$got" = rc=130 ] && [ "$took" -lt 15 ]; then
        ok "terminal: Ctrl-C interrupts the host process (exit 130 after ${took}s)"
    else
        bad "terminal: Ctrl-C" "got '$got' after ${took}s"
    fi
else
    skip "terminal forwarding" "no script(1) on the host"
fi

# ---- host-spawn command line ----
expect "host-spawn -no-pty -cwd -env" "$HOME|hello|xterm" \
    sh -c "TERM=xterm FPS_E2E_VAR=hello exec host-spawn -no-pty -cwd '$HOME' -env TERM,FPS_E2E_VAR fps-test-env"
expect "host-spawn passes only -env (default TERM)" "$HOME|unset|xterm" \
    sh -c "TERM=xterm FPS_E2E_VAR=hello exec host-spawn -cwd '$HOME' fps-test-env"
expect "flatpark-spawn passes the caller's environment" "$HOME|hello|xterm" \
    sh -c "TERM=xterm FPS_E2E_VAR=hello exec $SPAWN -cwd '$HOME' fps-test-env"
v=$(run host-spawn -version 2>&1)
case $v in ?*) ok "host-spawn -version ($v)" ;; *) bad "host-spawn -version" "empty" ;; esac
expect "host-spawn without a command opens \$SHELL" "host-shell-ok" sh -c 'echo "echo host-shell-ok; exit" | host-spawn -no-pty'

# ---- without the permission ----
# FLATPARK_SPAWN_CONFIG is the e2e's own (finish-args, see run.sh), not
# enable.sh's.
out=$(flatpak run --no-talk-name=org.freedesktop.Flatpak "$APP" sh -c 'env' 2>"$T/err" | grep -v '^FLATPARK_SPAWN_CONFIG=')
if ! printf '%s\n' "$out" | grep -qE '^(FLATPARK_SPAWN_|LD_PRELOAD=|ZYPAK_LD_PRELOAD=|BASH_FUNC_)' && [ ! -s "$T/err" ]; then
    ok "no permission: no FLATPARK_SPAWN_*, LD_PRELOAD, BASH_FUNC_*; no output"
else
    bad "no permission: environment" "$(printf '%s\n' "$out" | grep -E '^(FLATPARK|LD_|ZYPAK|BASH_FUNC)')" "$(cat "$T/err")"
fi
flatpak run --no-talk-name=org.freedesktop.Flatpak "$APP" $P execvp fps-test-hostonly >"$T/out" 2>"$T/err"
rc=$?
if [ "$rc" = 111 ] && [ "$(cat "$T/err")" = "execvp: No such file or directory" ]; then
    ok "no permission: a missing command is the native ENOENT"
else
    bad "no permission: missing command" "exit $rc" "$(cat "$T/err")"
fi
got=$(flatpak run --no-talk-name=org.freedesktop.Flatpak --env=LC_ALL=C "$APP" sh -c fps-test-hostonly 2>&1)
case $got in
*"fps-test-hostonly: command not found") ok "no permission: shell says the native 'command not found'" ;;
*) bad "no permission: shell" "$got" ;;
esac

# ---- zypak ----
zw=$(run sh -c 'command -v zypak-wrapper.sh || command -v zypak-wrapper' 2>/dev/null)
if [ -n "$zw" ]; then
    py="import subprocess, sys; sys.exit(subprocess.run(['fps-test-hostonly']).returncode)"
    expect "zypak: ZYPAK_LD_PRELOAD carries the library into the wrapped process" host-only-ok \
        "$zw" /usr/bin/python3 -c "$py"
    run sh -c "unset ZYPAK_LD_PRELOAD; exec $zw /usr/bin/python3 -c \"$py\"" >"$T/out" 2>&1
    if grep -q host-only-ok "$T/out"; then
        echo "note - zypak kept LD_PRELOAD here as well (control case passed without ZYPAK_LD_PRELOAD)"
    else
        ok "zypak: without ZYPAK_LD_PRELOAD the library is gone (control)"
    fi
else
    skip "zypak" "no zypak-wrapper in the app"
fi

# ---- socket created on the host ----
if python3 -c 'import socket; socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)' 2>/dev/null; then
    vs="import socket
try:
    s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
    print('family', s.getsockopt(socket.SOL_SOCKET, socket.SO_DOMAIN))
except OSError as e:
    print('errno', e.errno)"
    expect "AF_VSOCK through the preload: created on the host" "family 40" python3 -c "$vs"
    # /usr/bin/python3, not python3: a bare python3 from a preloaded env
    # would itself run on the host (which has python3).
    expect "AF_VSOCK bypassing the preload: EAFNOSUPPORT (97)" "errno 97" env -u LD_PRELOAD /usr/bin/python3 -c "$vs"
else
    skip "AF_VSOCK" "the host itself cannot create AF_VSOCK sockets"
fi

# ---- the host configuration (config.yaml) and shims ----
# The agent reads $CFG, a file of the e2e's own: the test apps carry
# FLATPARK_SPAWN_CONFIG pointing at it (run.sh), so no check reads or
# writes the user's ~/.config/flatpark-spawn. Up to here there is no such
# file (0.1's behaviour); it is removed again at the end of this section.
# $INC is a host directory only include_path names (fps-test-incl).
setcfg() { printf '%s\n' "$@" >"$CFG"; }
# A name no one has: the agent answers it, which is the next query after a
# change of the file; the sandbox's caches start over from there.
query() { echo "$P spawnp fps-test-query-$$-$1 >/dev/null 2>&1;"; }
host_ok_rc() { # name CMD...: not forwarded, ENOENT as exec-probe reports it
    name=$1
    shift
    run "$@" >"$T/out" 2>"$T/err"
    rc=$?
    if [ "$rc" = 111 ] && grep -q 'No such file or directory' "$T/err" && [ ! -s "$T/out" ]; then
        ok "$name"
    else
        bad "$name" "want exit 111 and ENOENT, got $rc" "stdout: $(head -c 200 "$T/out")" "stderr: $(head -c 300 "$T/err")"
    fi
}

if [ -n "$usrcmd" ]; then
    want=$("/usr/bin/$usrcmd" --version 2>/dev/null)
    noshim=fps-test-noshim-$$
    setcfg "shims: [$usrcmd, $noshim]"
    # enable.sh makes the shims at app start and says which it could not.
    got=$(run sh -c 'for f in "$FLATPARK_SPAWN_SHIMS"/*; do printf "%s " "${f##*/}"; done; case :$PATH: in *:"$FLATPARK_SPAWN_SHIMS":*) echo started ;; esac' 2>"$T/err")
    if [ "$got" = "$usrcmd started" ] && grep -q "shim $noshim: not found in the host path (checked: " "$T/err"; then
        ok "shims: $usrcmd made at app start and in PATH; $noshim (not on the host) not made, said on stderr; the app starts"
    else
        bad "shims at app start" "got: $got" "stderr: $(head -c 300 "$T/err")"
    fi
    expect "shims: sh without LD_PRELOAD runs $usrcmd by name, on the host" "$want" \
        env -u LD_PRELOAD /bin/sh -c "$usrcmd --version"
    expect "shims: with the preload, execvp $usrcmd is forwarded as before" "$want" $P execvp "$usrcmd" --version
    if [ -n "$node" ] && [ -x "$node" ]; then
        nodebin=$(readlink -f "$node")
        nodedir=$(dirname "$(dirname "$nodebin")")
        js=/app/share/spawn-test/node-strip.js
        nrun() { flatpak run --filesystem="$nodedir:ro" "$APP" "$nodebin" "$js" strip "$@"; }
        got=$(nrun spawn "$usrcmd" --version 2>"$T/err")
        if [ "$got" = "$want" ] && grep -q 'node-strip spawn: preload not loaded' "$T/err"; then
            ok "shims: node child_process in a child node without LD_PRELOAD (VS Code's extension host) runs $usrcmd on the host"
        else
            bad "shims: node child without LD_PRELOAD, child_process" "got: $got" "stderr: $(head -c 300 "$T/err")"
        fi
        got=$(nrun exists "$usrcmd" 2>/dev/null)
        if [ "$got" = true ]; then ok "shims: fs.existsSync(<shim directory>/$usrcmd) is true"; else bad "shims: existsSync" "got: $got"; fi
        got=$(nrun find "$usrcmd" --version 2>"$T/err")
        if [ "$got" = "$want" ]; then
            ok "shims: found on disk in PATH and run, as vscode-neovim's findNvim does, without LD_PRELOAD"
        else
            bad "shims: findNvim-style lookup" "got: $got" "stderr: $(head -c 300 "$T/err")"
        fi
    else
        skip "shims from node" "no node on the host to run in the sandbox"
    fi
    run "$SPAWN" check >"$T/out" 2>"$T/err"
    rc=$?
    if [ "$rc" = 0 ] || { [ "$rc" = 1 ] && grep -q "✗ $noshim" "$T/out"; }; then
        if grep -q "✓ $usrcmd" "$T/out" && grep -q "in this app" "$T/out"; then
            ok "flatpark-spawn check in the app: $usrcmd found, $noshim not (exit $rc), the app's shim directory listed"
        else
            bad "check in the app" "$(head -c 600 "$T/out")"
        fi
    else
        bad "check in the app" "exit $rc" "$(head -c 600 "$T/out")" "stderr: $(head -c 300 "$T/err")"
    fi
else
    skip "shims" "no command the host has in /usr/bin and $APP lacks (see the absolute path checks)"
fi

# exclude_path: neither by name nor by absolute path. fps-test-hostonly was
# forwarded from this directory above, so the sandbox has it cached as on
# the host; the first query after the change empties that.
setcfg "exclude_path: [~/.local/bin]"
host_ok_rc "exclude_path ~/.local/bin: fps-test-hostonly by name is no longer forwarded (ENOENT)" \
    sh -c "$(query 1) exec $P execvp fps-test-hostonly"
run $P execve "$HOME/.local/bin/fps-test-hostonly" >"$T/out" 2>"$T/err"
rc=$?
if [ "$rc" = 127 ] && grep -q "fps-test-hostonly: No such file or directory" "$T/err" && [ ! -s "$T/out" ]; then
    ok "exclude_path ~/.local/bin: the absolute path is \"not on the host\" either: 127 + ENOENT message"
else
    bad "exclude_path, absolute path" "exit $rc" "stdout: $(head -c 200 "$T/out")" "stderr: $(head -c 300 "$T/err")"
fi

# use_login_shell_path: false: only include_path, which is also the
# command's PATH.
setcfg "use_login_shell_path: false" "include_path: [$INC]"
expect "use_login_shell_path: false + include_path: a command there runs, with that PATH" "incl-ok PATH=$INC" \
    $P execvp fps-test-incl
host_ok_rc "use_login_shell_path: false: a command only the login PATH has is not forwarded" $P execvp fps-test-hostonly

# A change of the file reaches a running app at its next query: one app
# instance forwards fps-test-hostonly, the file changes meanwhile, a query,
# and the same name no longer goes to the host.
nocfg_and_go=$WORK/config/go
rm -f "$CFG" "$nocfg_and_go"
flatpak run --filesystem="$WORK/config:ro" "$APP" sh -c "$P execvp fps-test-hostonly
    while [ ! -e '$nocfg_and_go' ]; do sleep 0.1; done
    $(query 2) $P execvp fps-test-hostonly; echo rc=\$?" >"$T/live" 2>"$T/err" &
live=$!
for _ in $(seq 100); do grep -q host-only-ok "$T/live" 2>/dev/null && break; sleep 0.1; done
setcfg "exclude_path: [~/.local/bin]"
touch "$nocfg_and_go"
wait "$live"
if [ "$(tr '\n' ' ' <"$T/live")" = "host-only-ok rc=111 " ]; then
    ok "config.yaml changed while the app runs: the next query follows it (no restart)"
else
    bad "config change in a running app" "got: $(tr '\n' ' ' <"$T/live")" "stderr: $(head -c 300 "$T/err")"
fi
rm -f "$nocfg_and_go"

# An error in the file: check names the line and fails; forwarding goes on
# as without the file, with a warning.
setcfg "include_paths: [/opt/x]"
run "$SPAWN" check >"$T/out" 2>"$T/err"
rc=$?
if [ "$rc" = 1 ] && grep -q "config.yaml:1: unknown key \"include_paths\" (did you mean include_path?)" "$T/out"; then
    ok "config.yaml with an unknown key: check reports file:line and exits 1"
else
    bad "check with an error" "exit $rc" "$(head -c 400 "$T/out")" "stderr: $(head -c 300 "$T/err")"
fi
setcfg "shims: [a, b" "exclude_path: [~/.local/bin]"
got=$(run sh -c "$(query 3) exec $P execvp fps-test-hostonly" 2>"$T/err")
if [ "$got" = host-only-ok ] && grep -q "ignoring the configuration" "$T/err"; then
    ok "config.yaml with a YAML syntax error: forwarded as without the file (its exclude_path ignored), with a warning"
else
    bad "forwarding with a broken file" "got: $got" "stderr: $(head -c 300 "$T/err")"
fi
run "$SPAWN" check >"$T/out" 2>&1
if grep -q 'config.yaml:[0-9]*: YAML syntax' "$T/out"; then
    ok "check reports the YAML syntax error with its line"
else
    bad "check: syntax error" "$(head -c 400 "$T/out")"
fi

# Back to no file: the next app start removes the shims and leaves PATH.
rm -f "$CFG"
expect "no config.yaml again: no shim directory, PATH as without flatpark-spawn's shims" "none" \
    sh -c '[ -e "$FLATPARK_SPAWN_SHIMS" ] && echo "exists" || case :$PATH: in *:"$FLATPARK_SPAWN_SHIMS":*) echo "in PATH" ;; *) echo none ;; esac'

echo "-- $APP: $pass ok, $failed failed, $skipped skipped"
[ "$failed" = 0 ]
