#!/bin/sh
# Medians, measured inside the app with exec-probe's bench (posix_spawn +
# waitpid per run). Targets: forward of `true` with warm caches <= 6 ms
# (first 5 ms; relaxed when the pipelined D-Bus connection was dropped, see
# DESIGN.md, 性能); a bare name the host lacks, from the second call on,
# <= 3 ms; local exec overhead of the preload <= 100 us.
# Since 0.2 the command starts about 0.2-0.3 ms slower (the package init of
# gopkg.in/yaml.v3 and encoding/json, linked in for config.yaml), which the
# forward pays once even on a cache hit: an A/B in one sandbox measured
# 5.29 -> 5.68 ms (median of 20 rounds of 50). Accepted; the targets are
# unchanged (DESIGN.md, 性能).
# Usage: WORK=dir perf.sh APP-ID
set -u

APP=$1
P=/app/libexec/spawn-test/exec-probe
NOOP=/app/libexec/spawn-test/noop
run() { flatpak run "$APP" "$@"; }
failed=0

# Fill the caches: login PATH on the host, the app's lookup cache.
run $P spawnp true >/dev/null 2>&1
run $P spawnp fps-own-tool -q >/dev/null 2>&1
run $P spawnp fps-missing-everywhere >/dev/null 2>&1

fwd=$(run $P bench 50 spawnp true)
neg=$(run $P bench 50 spawnp fps-own-tool -q)
neg_base=$(run env -u LD_PRELOAD $P bench 50 spawnp fps-own-tool -q)
missing=$(run $P bench 50 spawnp fps-missing-everywhere)
loc=$(run $P bench 300 spawn $NOOP)
loc_base=$(run env -u LD_PRELOAD $P bench 300 spawn $NOOP)
delta=$((loc - loc_base))

# A busy machine (7.9 ms and 209 us were measured under load) must not
# fail the run: over the target is reported as "slow"; only three times the
# target fails, which no load explains and a regression (a login shell per
# call, a lost cache) does.
report() { # name value-us limit-us
    if [ "$2" -le "$3" ]; then
        r=ok
    elif [ "$2" -le $(($3 * 3)) ]; then
        r=slow
    else
        r=FAIL failed=1
    fi
    printf '%-4s - %-62s %7s us (target <= %s us, fails over %s)\n' "$r" "$1" "$2" "$3" "$(($3 * 3))"
}
report "forward \`true\` to the host (cache hit)" "$fwd" 6000
report "bare name only the sandbox has, 2nd+ call (negative cache)" "$neg" 3000
printf '       same without the preload: %s us\n' "$neg_base"
report "bare name nobody has, 2nd+ call (ENOENT)" "$missing" 3000
report "local exec overhead (absolute path, preload vs none)" "$delta" 100
printf '       %s us with the preload, %s us without\n' "$loc" "$loc_base"
[ "$failed" = 0 ]
