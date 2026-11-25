#!/bin/sh
# Fails when a shared object needs a glibc symbol version newer than the cap.
# Usage: check-glibc.sh lib.so 2.34
set -eu

so=$1
max=$2
versions=$(readelf -W --dyn-syms --version-info "$so" |
    grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/^GLIBC_//' | sort -u -V)
highest=$(printf '%s\n' "$versions" | tail -n1)
[ -n "$highest" ] || { echo "check-glibc: no GLIBC versions in $so" >&2; exit 1; }
top=$(printf '%s\n%s\n' "$highest" "$max" | sort -V | tail -n1)
if [ "$top" != "$max" ]; then
    echo "check-glibc: $so needs GLIBC_$highest, cap is GLIBC_$max" >&2
    readelf -W --dyn-syms "$so" | grep "GLIBC_$highest" >&2 || true
    exit 1
fi
echo "check-glibc: $so highest GLIBC_$highest (cap GLIBC_$max)"
