#!/bin/sh
# make e2e: build the payload (Makefile), the extension and two consumer
# apps (freedesktop 26.08, GNOME 51); serve the payload over a local HTTP
# server as extra-data; install the apps from a temporary --user remote
# (the extension must come along on its own); run the matrix and the
# timings in both; remove everything it added, also when it fails.
#
# E2E_KEEP=1 leaves everything installed for debugging; `run.sh --clean`
# removes it.
#
# It is kept apart from anything else on the machine, in particular from a
# real org.flatpark.Spawn//1 and the apps using it (such as local demo
# builds, org.flatpark.SpawnDemo.*): everything is built and installed under its
# own ids (org.flatpark.SpawnE2E*, rewritten from packaging/ and tests/e2e/apps
# at build time), from its own remote and work directory. A run that would
# change org.flatpark.Spawn or org.flatpark.SpawnDemo.* fails (checked at the
# end).
#
# Touches the host only in these places, all removed on exit:
#   --user refs  org.flatpark.SpawnE2E//1, org.flatpark.SpawnE2E.{Fd,Gnome},
#                org.flatpark.SpawnE2E.BuildParent, and
#                org.electronjs.Electron2.BaseApp//26.08 if it was missing
#   remote       flatpark-spawn-e2e-run
#   files        ~/.local/bin/fps-test-*, ~/.var/app/<test ids>,
#                ~/.cache/fps-test-mise.* and the links mise makes to it in
#                ~/.local/state/mise/tracked-configs,
#                $XDG_RUNTIME_DIR/app/<test ids>, .build/e2e,
#                $XDG_RUNTIME_DIR/flatpark-spawn if it did not exist (if it
#                did, only the entries of the temporary mise project)
set -eu

ROOT=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
WORK=$ROOT/.build/e2e-run
ARCH=$(uname -m)
REMOTE=flatpark-spawn-e2e-run
EXT=org.flatpark.SpawnE2E
APPS="$EXT.Fd $EXT.Gnome"
PARENT=$EXT.BuildParent
# Never touched; compared before and after.
PROTECTED='^org\.flatpark\.(Spawn|SpawnDemo\.[^/]*)/'
protected_state() {
    flatpak --user list --all --columns=ref,origin,active 2>/dev/null | grep -E "$PROTECTED" || true
    flatpak --user remotes --columns=name,url 2>/dev/null | grep -E '^flatpark-spawn-e2e[[:space:]]' || true
}
PROTECTED_BEFORE=$(protected_state)
BASEAPP=app/org.electronjs.Electron2.BaseApp/$ARCH/26.08
RUNDIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
HOSTCACHE=$RUNDIR/flatpark-spawn
HOSTBIN=$HOME/.local/bin
TARBALL=${TARBALL:-$(ls -t "$ROOT"/.build/dist/flatpark-spawn-*-"$ARCH".tar.gz 2>/dev/null | head -n1)}

HTTP_PID=
# Nothing is removed on exit until the preconditions have passed: a run that
# stops because something it would create already exists (and so is not
# its own) must not delete that thing on the way out.
STARTED=0
INSTALLED_BASEAPP=0
CREATED_HOSTCACHE=0
[ -e "$HOSTCACHE" ] || CREATED_HOSTCACHE=1

log() { printf '\n== %s\n' "$*"; }
die() { echo "e2e: $*" >&2; exit 1; }

uninstall_test_refs() {
    for a in $APPS $PARENT; do
        flatpak --user uninstall -y --noninteractive --delete-data "app/$a/$ARCH/master" >/dev/null 2>&1 || true
    done
    flatpak --user uninstall -y --noninteractive "runtime/$EXT/$ARCH/1" >/dev/null 2>&1 || true
}

cleanup() {
    rc=$?
    set +e
    trap - EXIT HUP INT TERM
    if [ "$STARTED" != 1 ]; then
        echo "e2e: stopped before starting, nothing changed"
        exit "$rc"
    fi
    if [ "${E2E_KEEP-}" = 1 ] && [ "${CLEANING-}" != 1 ]; then
        [ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
        echo "e2e: E2E_KEEP=1, left installed; remove with: tests/e2e/run.sh --clean"
        exit "$rc"
    fi
    log "cleanup"
    [ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
    uninstall_test_refs
    flatpak --user remote-delete --force "$REMOTE" >/dev/null 2>&1
    if [ "$INSTALLED_BASEAPP" = 1 ]; then
        flatpak --user uninstall -y --noninteractive "$BASEAPP" >/dev/null 2>&1
    fi
    pkill -f "$HOSTBIN/fps-test-sleep" 2>/dev/null
    rm -f "$HOSTBIN"/fps-test-*
    # Host login-PATH cache entries for the temporary mise projects (the
    # cache itself may be shared with apps really using flatpark-spawn).
    for f in "$HOSTCACHE"/login-path.d/*; do
        case "$(head -n1 "$f" 2>/dev/null)" in "$HOME"/.cache/fps-test-mise.*) rm -f "$f" ;; esac
    done
    # matrix.sh's temporary mise project, if it was interrupted
    for d in "$HOME"/.cache/fps-test-mise.*; do
        [ -d "$d" ] || continue
        for l in "$HOME"/.local/state/mise/tracked-configs/*; do
            case "$(readlink "$l" 2>/dev/null)" in "$d"/*) rm -f "$l" ;; esac
        done
        rm -rf "$d"
    done
    for a in $APPS $PARENT; do
        rm -rf "$HOME/.var/app/$a" "$RUNDIR/app/$a"
    done
    [ "$CREATED_HOSTCACHE" = 1 ] && rm -rf "$HOSTCACHE"
    rm -rf "$WORK"
    left=$(flatpak --user list --all --columns=ref | grep -E "^${EXT}[./]" || true)
    [ -z "$left" ] || { echo "e2e: left installed: $left" >&2; rc=1; }
    if flatpak --user remotes --columns=name | grep -qx "$REMOTE"; then
        echo "e2e: remote $REMOTE left" >&2
        rc=1
    fi
    if [ "$(protected_state)" != "$PROTECTED_BEFORE" ]; then
        echo "e2e: org.flatpark.Spawn / org.flatpark.SpawnDemo.* / remote flatpark-spawn-e2e changed:" >&2
        printf 'before:\n%s\nafter:\n%s\n' "$PROTECTED_BEFORE" "$(protected_state)" >&2
        rc=1
    fi
    [ "$rc" = 0 ] && echo "e2e: clean" || echo "e2e: exit $rc"
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

# --clean: remove what an E2E_KEEP=1 run left (only org.flatpark.SpawnE2E*,
# remote flatpark-spawn-e2e-run and .build/e2e-run). Whether BaseApp was
# installed by the e2e is not remembered across runs; it is left alone.
if [ "${1-}" = --clean ]; then
    CLEANING=1
    STARTED=1
    CREATED_HOSTCACHE=0 # shared with real users of flatpark-spawn: only our entries go
    exit 0
fi

builder() {
    flatpak run --env=FLATPAK_USER_DIR="$HOME/.local/share/flatpak" \
        --command=flatpak-builder org.flatpak.Builder \
        --state-dir="$WORK/state" --disable-rofiles-fuse --force-clean \
        --repo="$WORK/repo" "$@"
}

# ---- preconditions ----
[ -n "$TARBALL" ] && [ -r "$TARBALL" ] || die "no payload; run make e2e"
command -v python3 >/dev/null || die "python3 is needed for the local HTTP server"
flatpak info --user org.flatpak.Builder >/dev/null 2>&1 ||
    flatpak info org.flatpak.Builder >/dev/null 2>&1 || die "org.flatpak.Builder is not installed"
for r in org.freedesktop.Platform//26.08 org.freedesktop.Sdk//26.08 org.gnome.Platform//51 org.gnome.Sdk//51; do
    flatpak info "$r" >/dev/null 2>&1 || die "$r is not installed"
done
ls "$HOSTBIN"/fps-test-* >/dev/null 2>&1 && die "$HOSTBIN/fps-test-* exist and are not ours; remove them first"
ls -d "$HOME"/.cache/fps-test-mise.* >/dev/null 2>&1 && die "$HOME/.cache/fps-test-mise.* exist and are not ours; remove them first"

STARTED=1
rm -rf "$WORK"
mkdir -p "$WORK"
uninstall_test_refs
flatpak --user remote-delete --force "$REMOTE" >/dev/null 2>&1 || true

# ---- payload over HTTP ----
log "payload $(basename "$TARBALL")"
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$(dirname "$TARBALL")" >"$WORK/http.log" 2>&1 &
HTTP_PID=$!
SHA=$(sha256sum "$TARBALL" | cut -d' ' -f1)
SIZE=$(stat -c %s "$TARBALL")
URL="http://127.0.0.1:$PORT/$(basename "$TARBALL")"
for _ in 1 2 3 4 5 6 7 8 9 10; do
    python3 -c "import urllib.request; urllib.request.urlopen('$URL', timeout=1)" 2>/dev/null && break
    sleep 0.2
done

if ! flatpak info --user "$BASEAPP" >/dev/null 2>&1 && ! flatpak info --system "$BASEAPP" >/dev/null 2>&1; then
    log "installing $BASEAPP (removed again at the end)"
    flatpak --user install -y --noninteractive flathub "$BASEAPP" >/dev/null
    INSTALLED_BASEAPP=1
fi

# ---- the extension, built against its stand-in parent ----
mkdir -p "$WORK/ext" "$WORK/apps" "$WORK/build-parent-src"
cp "$ROOT/packaging/apply_extra.sh" "$WORK/ext/"
# The real manifests, under the e2e's own ids.
to_e2e() { sed -E 's/org\.flatpark\.Spawn\b/org.flatpark.SpawnE2E/g' "$1" >"$2"; }
to_e2e "$ROOT/packaging/org.flatpark.Spawn.metainfo.xml" "$WORK/ext/$EXT.metainfo.xml"
to_e2e "$ROOT/packaging/build-parent/org.flatpark.Spawn.BuildParent.yml" "$WORK/build-parent-src/$PARENT.yml"
for v in Fd Gnome; do
    sed -E -e 's/org\.flatpark\.SpawnTest\./org.flatpark.SpawnE2E./' \
        -e 's/^  org\.flatpark\.Spawn:$/  org.flatpark.SpawnE2E:/' \
        -e "s|path: \.\./fixtures|path: $ROOT/tests/e2e/fixtures|" \
        "$ROOT/tests/e2e/apps/org.flatpark.SpawnTest.$v.yml" >"$WORK/apps/$EXT.$v.yml"
done
log "building $PARENT (stand-in, never published)"
builder "$WORK/build-parent" "$WORK/build-parent-src/$PARENT.yml" >"$WORK/build-parent.log" 2>&1 ||
    { tail -20 "$WORK/build-parent.log"; die "building $PARENT failed"; }
flatpak build-update-repo "$WORK/repo" >/dev/null
flatpak --user remote-add --no-gpg-verify "$REMOTE" "$WORK/repo"
flatpak --user install -y --noninteractive "$REMOTE" "app/$PARENT/$ARCH/master" >/dev/null

log "building $EXT from packaging/org.flatpark.Spawn.yml with the local payload"
python3 - "$ROOT/packaging/org.flatpark.Spawn.yml" "$WORK/ext/$EXT.yml" "$URL" "$SHA" "$SIZE" "$ARCH" <<'PY'
import re, sys
src, dst, url, sha, size, arch = sys.argv[1:]
out, skipping = [], False
for line in open(src):
    if "# BEGIN MANAGED EXTRA-DATA" in line:
        out.append(line)
        out.append(f"      - type: extra-data\n        filename: flatpark-spawn.tar.gz\n"
                   f"        only-arches:\n          - {arch}\n        url: {url}\n"
                   f"        sha256: {sha}\n        size: {size}\n")
        skipping = True
    elif "# END MANAGED EXTRA-DATA" in line:
        skipping = False
        out.append(line)
    elif not skipping:
        out.append(line)
open(dst, "w").writelines(re.sub(r"org\.flatpark\.Spawn\b", "org.flatpark.SpawnE2E", l) for l in out)
PY
builder "$WORK/build-ext" "$WORK/ext/$EXT.yml" >"$WORK/build-ext.log" 2>&1 ||
    { tail -20 "$WORK/build-ext.log"; die "building $EXT failed"; }
flatpak --user uninstall -y --noninteractive "app/$PARENT/$ARCH/master" >/dev/null
echo "metadata:"
sed 's/^/  /' "$WORK/build-ext/metadata"

# ---- the consumer apps ----
for a in $APPS; do
    log "building $a"
    builder "$WORK/build-$a" "$WORK/apps/$a.yml" >"$WORK/build-$a.log" 2>&1 ||
        { tail -20 "$WORK/build-$a.log"; die "building $a failed"; }
done
flatpak build-update-repo "$WORK/repo" >/dev/null

log "installing the apps; $EXT must come along"
# shellcheck disable=SC2046
flatpak --user install -y --noninteractive "$REMOTE" $(for a in $APPS; do echo "app/$a/$ARCH/master"; done) >"$WORK/install.log" 2>&1 ||
    { cat "$WORK/install.log"; die "install failed"; }
flatpak --user info "runtime/$EXT/$ARCH/1" >/dev/null 2>&1 || { cat "$WORK/install.log"; die "$EXT was not installed with the apps"; }
echo "ok   - $EXT//1 installed with the apps, from $(flatpak --user info "runtime/$EXT/$ARCH/1" | sed -n 's/^ *Origin: //p')"
grep -q "GET /$(basename "$TARBALL")" "$WORK/http.log" && echo "ok   - payload fetched as extra-data from the local server"

# ---- host commands used by the matrix ----
mkdir -p "$HOSTBIN"
printf '#!/bin/sh\necho host-only-ok\n' >"$HOSTBIN/fps-test-hostonly"
printf '#!/bin/sh\necho ran\nexit 127\n' >"$HOSTBIN/fps-test-exit127"
# Not exec: the sleep child dies only if the signal reaches the process group.
printf '#!/bin/sh\nsleep 30\necho survived\n' >"$HOSTBIN/fps-test-sleep"
printf '#!/bin/sh\nprintf "%%s|%%s|%%s\\n" "$PWD" "${FPS_E2E_VAR-unset}" "${TERM-unset}"\n' >"$HOSTBIN/fps-test-env"
# What a host command sees of its terminal: a controlling terminal (field 7
# of /proc/self/stat, tty_nr), whether /dev/tty opens, the window size, and
# its stdin (a terminal, or the data piped in).
cat >"$HOSTBIN/fps-test-ttycheck" <<'SH'
#!/bin/sh
nr=$(sed 's/^.*) //' /proc/$$/stat | cut -d' ' -f5)
[ "$nr" != 0 ] && ctty=yes || ctty=no
if (exec </dev/tty) 2>/dev/null; then dev=ok; else dev=fail; fi
size=$( (stty size </dev/tty) 2>/dev/null)
if [ -t 0 ]; then in=tty; else in=$(cat); fi
echo "ctty=$ctty devtty=$dev size=$size stdin=$in"
exit 7
SH
printf '#!/bin/sh\nexec sleep 20\n' >"$HOSTBIN/fps-test-wait"
chmod 755 "$HOSTBIN"/fps-test-*
"${SHELL:-/bin/sh}" -lic 'command -v fps-test-hostonly' </dev/null >/dev/null 2>&1 ||
    die "$HOSTBIN is not in the login shell's PATH; the matrix needs it there"

failed=0
for a in $APPS; do
    log "matrix: $a"
    WORK=$WORK sh "$ROOT/tests/e2e/matrix.sh" "$a" || failed=1
done
for a in $APPS; do
    log "timings: $a (medians)"
    WORK=$WORK sh "$ROOT/tests/e2e/perf.sh" "$a" || failed=1
done

log "extension not installed"
flatpak --user uninstall -y --noninteractive "runtime/$EXT/$ARCH/1" >/dev/null
for a in $APPS; do
    out=$(flatpak run "$a" sh -c 'echo "${LD_PRELOAD-none} ${FLATPARK_SPAWN_ENABLE-none}"' 2>"$WORK/err")
    if [ "$out" = "none none" ] && [ ! -s "$WORK/err" ]; then
        echo "ok   - $a: launcher starts without the extension, nothing set, no output"
    else
        echo "FAIL - $a: without the extension: '$out' $(cat "$WORK/err")"
        failed=1
    fi
done

[ "$failed" = 0 ] || die "some checks failed"
log "all checks passed"
