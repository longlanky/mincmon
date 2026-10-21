#!/usr/bin/env bash
# Copyright (C) 2026 LongLanky
# SPDX-License-Identifier: GPL-3.0-or-later
#
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
mods=$(mktemp)
trap 'rm -f "$mods" "$mods.notices"' EXIT
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
    # Record the third-party modules linked into this target
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go list -deps \
        -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' \
        ./cmd/mincmon >> "$mods"
    unset GOAMD64 GOARM64
    printf '%-36s %s\n' "$out" "$(du -h "$out" | cut -f1)"
done

# Ship the license and the notices required by the bundled dependencies
# (MIT/BSD licenses require them to accompany binary distributions).
cp LICENSE dist/LICENSE
{
    echo "mincmon is licensed under the GNU GPL v3 or later (see LICENSE)."
    echo "Its binaries include the following third-party software."
    printf '\n==== Go standard library and runtime (%s) ====\n\n' "$(go env GOVERSION)"
    # Some distro packages (e.g. Gentoo's) omit GOROOT/LICENSE; golang.org/x/sys
    # carries the identical Go Authors BSD license and is always a dependency.
    golic="$(go env GOROOT)/LICENSE"
    [ -f "$golic" ] || golic="$(go list -m -f '{{.Dir}}' golang.org/x/sys)/LICENSE"
    cat "$golic"
    sort -u "$mods" | while read -r path version dir; do
        printf '\n==== %s %s ====\n\n' "$path" "$version"
        lic=$(find "$dir" -maxdepth 1 -type f -iregex '.*/\(licen[cs]e\|copying\)\(\.md\|\.txt\)?' | head -1)
        readme=$(find "$dir" -maxdepth 1 -type f -iname 'readme*' | head -1)
        if [ -n "$lic" ]; then
            cat "$lic"
        elif [ -n "$readme" ] && grep -qi '^#* *licen[cs]e' "$readme"; then
            # e.g. mattn/go-localereader: MIT per README, no LICENSE file
            echo "(This module ships no license file; its README states:)"
            echo
            sed -n '/^#* *[Ll]icen[cs]e/,$p' "$readme"
        else
            echo "no license information found for $path" >&2
            exit 1
        fi
    done
} > "$mods.notices"
mv "$mods.notices" dist/THIRD_PARTY_NOTICES.txt
echo "dist/LICENSE, dist/THIRD_PARTY_NOTICES.txt"
