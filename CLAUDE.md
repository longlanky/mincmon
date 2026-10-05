# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

mincmon is a network ping monitor with a live terminal UI, written in Go (v4) under `cmd/` and `internal/`. It sends ICMP natively and builds to static binaries for Linux/Windows/macOS. It is a rewrite of an earlier Python tool (since removed); some comments refer to that version's behavior.

## Commands

```bash
go run ./cmd/mincmon [flags] [IP|CIDR|domain ...]   # no targets → interactive prompts
go test ./...                                      # resolve/probe tests use the network; skip if unavailable
go vet ./... && GOOS=windows go vet ./... && GOOS=darwin go vet ./...
./build.sh [os/arch[/level] ...]                   # static CGO_ENABLED=0 binaries → dist/mincmon-<os>-<arch>[-<level>]
```

Flags: `-f list.ml.txt`, `-i 2s`, `-t 1s`, `--resolver IPs|system`, `--records A|AAAA|both`, `--backend auto|icmp|raw|exec`. Go's `flag` stops at the first positional argument, so flags must come before the targets.

`dist/` is build output (gitignored). Don't edit it.

## Go architecture

- `internal/targets` covers input classification (`Classify`/`ParseEntries`), refinement parsing (`ParseRefine`: decimal for IPv4, hex or `:`-grouped hex for IPv6), and subnet expansion (`Expand`, capped at `HostCap` = 1024). It uses a small `u128` for IPv6 offset math. `Dedupe` merges domain labels for IPs shared by several domains.
- `internal/probe` has a `Prober` interface. Backends are chosen per address family by `New`, in the order unprivileged ICMP datagram socket (Linux needs `net.ipv4.ping_group_range`; macOS allows it by default), then the Windows `IcmpSendEcho`/`Icmp6SendEcho2` API, then raw ICMP socket (root/`CAP_NET_RAW`), then the system `ping` binary. The socket backends share **one socket per family**; a receiver goroutine matches replies to waiting probes by (source addr, seq). Datagram sockets get their ICMP ID rewritten by the kernel, so the ID is only checked on raw sockets.
- `internal/monitor` runs one goroutine per target, each with its own context cancel. Start times are staggered (`seq%40 * interval/40`). Each host keeps a `sync.RWMutex`-guarded table in insertion order, plus a 60-entry RTT history (negative = lost). `Snapshot()` returns copies.
- `internal/resolve` queries user-given resolvers via `miekg/dns` (no `/etc/hosts` or search domains), or falls back to `net.DefaultResolver`.
- `internal/store` handles `.ml.txt` load/save.
- `internal/app` holds the interactive flows (`ResolveEntries`, `Add`, `Remove`, `Save`, `Load`, `PromptLoad`), written against a `Prompter` interface. `cmd/mincmon` implements it with stdin at startup; `internal/tui` implements it by sending a `promptReq` message to Bubble Tea and blocking on a reply channel. **Put new interactive logic in `app`, not the TUI**, so both paths share it.
- `internal/tui` is the Bubble Tea UI: a scrollable, sortable (`o`/`O`), filterable (`/`) table with sparklines. The keys `a r d s l q` are the actions. It splits multi-rune key messages into single keys, because fast typing arrives batched.

## Invariants

- `.ml.txt` rows are `ip,domain`, split on the *first* comma only, because the domain field may contain merged labels like `a.com, b.com`. `infra.ml.txt` is a real saved monitor list kept locally (`*.ml.txt` is gitignored because the repo is public); `internal/store` round-trips it byte-for-byte when present and skips otherwise.
- Expansion without a refinement mirrors the original Python version's `ipaddress.hosts()`: IPv4 skips network and broadcast (except /31); IPv6 skips the subnet-router anycast address (except /127).

## Constraints

- Windows support is maintained: `native_windows.go` (ICMP API) and the Windows branch in `exec.go`. Keep both paths, and vet with `GOOS=windows`. The Windows ICMPv6 reply offsets (Status at 28, RTT at 32) were not tested on real Windows.
- Release binaries must stay `CGO_ENABLED=0` (static, no libc) so they run on EL8 and older distros.
- `build.sh` must keep pinning `GOAMD64` (default v1) and `GOARM64`. This machine's Gentoo Go toolchain defaults to `GOAMD64=v3`, so unpinned binaries crash on older CPUs. Verify with `go version -m <binary>` or `qemu-x86_64 -cpu qemu64 <binary> -version`.
