#!/usr/bin/env bash
# Cross-compile static mincmon binaries into dist/. No container needed:
# CGO_ENABLED=0 binaries have no libc dependency (run on EL8 and newer).
#
#   ./build.sh                       # all targets
#   ./build.sh linux/amd64           # just the listed targets
#   ./build.sh linux/amd64/v2        # amd64 at a specific microarchitecture level
#
# The amd64 level is always pinned (default v1, runs on any x86-64 CPU):
# some Go toolchains (e.g. Gentoo's) default GOAMD64 to the build host's
# level, which yields binaries that crash on older CPUs.
set -euo pipefail
cd "$(dirname "$0")"

targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
    targets=(linux/amd64/v1 linux/amd64/v2 linux/arm64
             windows/amd64 windows/arm64 darwin/amd64 darwin/arm64)
fi

mkdir -p dist
for t in "${targets[@]}"; do
    IFS=/ read -r os arch level <<< "$t"
    out="dist/mincmon-$os-$arch"
    [ -n "$level" ] && out+="-$level"
    [ "$os" = windows ] && out+=".exe"
    case $arch in
        amd64) export GOAMD64=${level:-v1} ;;
        arm64) export GOARM64=${level:-v8.0} ;;
        *)     [ -z "$level" ] || { echo "no level support for $arch" >&2; exit 1; } ;;
    esac
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/mincmon
    unset GOAMD64 GOARM64
    printf '%-36s %s\n' "$out" "$(du -h "$out" | cut -f1)"
done
