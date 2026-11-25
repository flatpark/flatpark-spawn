#!/bin/sh
# Builds the test app's files (run by flatpak-builder in the Sdk).
set -eu
d=/app/libexec/spawn-test
# The consumer's one build line: the extension's mount point must exist.
mkdir -p /app/lib/flatpark-spawn
mkdir -p "$d/shim" /app/share/spawn-test
cc -O2 -Wall -Wextra -o "$d/exec-probe" exec-probe.c
cc -O2 -o "$d/noop" noop.c
cc -O2 -o /app/bin/fps-own-tool own-tool.c
install -m755 spawn-test /app/bin/spawn-test
install -m644 node-spawn.js /app/share/spawn-test/node-spawn.js
echo sandbox-marker > /app/share/spawn-test/marker.txt
# As the VS Code package will: host-spawn's name for existing terminal configs.
ln -s /app/lib/flatpark-spawn/bin/flatpark-spawn /app/bin/host-spawn
# host-spawn's shim: a symlink named after a host command runs it there.
ln -s /app/lib/flatpark-spawn/bin/flatpark-spawn "$d/shim/fps-test-hostonly"
