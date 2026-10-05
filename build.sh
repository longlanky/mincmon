#!/usr/bin/env bash
# Cross-compile static mincmon binaries into dist/. No container needed:
# CGO_ENABLED=0 binaries have no libc dependency (run on EL8 and newer).
#
#   ./build.sh                 # all targets
#   ./build.sh linux/amd64     # just the listed targets
set -euo pipefail
cd "$(dirname "$0")"

targets=("$@")
if [ ${#targets[@]} -eq 0 ]; then
    targets=(linux/amd64 linux/arm64 windows/amd64 windows/arm64 darwin/amd64 darwin/arm64)
fi

mkdir -p dist
for t in "${targets[@]}"; do
    os=${t%/*} arch=${t#*/}
    out="dist/mincmon-$os-$arch"
    [ "$os" = windows ] && out+=".exe"
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch \
        go build -trimpath -ldflags="-s -w" -o "$out" ./cmd/mincmon
    printf '%-32s %s\n' "$out" "$(du -h "$out" | cut -f1)"
done
