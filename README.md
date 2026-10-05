<h1 align="center">
  <img src="mincmon-1.png" alt="mincmon" width="300">
</h1>

<p align="center"><b>A fast, portable network ping monitor with a live terminal UI.</b></p>

Give it IP addresses, subnets, or domain names. mincmon pings every host continuously and shows a live table of status, latency, packet loss, and a recent-latency history graph per host. It ships as one static binary with no runtime dependencies, for Linux, Windows, and macOS.

```
mincmon v4.0   probe: ICMP socket   interval 2s   timeout 1s   2026-10-05 15:09:22

  STATUS   IP ADDRESS            DOMAIN                LATENCY   LOSS     UP   DOWN  HISTORY
──────────────────────────────────────────────────────────────────────────────────────────────────
▶ ● UP     192.0.2.10            web.example.com      23.41 ms     0%    128      0  ▃▄▃▅▃▄▃▃█▄▃▃
  ● UP     192.0.2.11            db.example.com       24.02 ms     5%    125      3  ▃▃×▃▄▃▃▅▃▃▄▃
  ○ DOWN   192.0.2.12                                        -   100%      0    128  ××××××××××××
  ● UP     2001:db8::10          web.example.com      25.10 ms     0%    128      0  ▄▃▃▄▃▅▃▃▄▃▃▄
──────────────────────────────────────────────────────────────────────────────────────────────────
UP: 3   DOWN: 1   PENDING: 0   TOTAL: 4   sort: added
192.0.2.10 web.example.com UP for 4m16s (since 15:05:06)

a add  r remove  d delete sel  s save  l load  o/O sort  / filter  ↑↓ select  q quit
```

## Features

- **Native ICMP:** sends echo requests itself instead of running a `ping` process per probe. All hosts share one socket per address family, so a thousand hosts cost about as much CPU as a handful.
- **IPv4 and IPv6** everywhere, including link-local addresses with zones (`fe80::1%eth0`).
- **Subnets with refinement:** enter a CIDR, then pick exactly which host offsets or ranges to watch.
- **Domains:** resolves A and/or AAAA records, from the system resolver or from DNS servers you choose. Each IP gets labeled with its domain.
- **Live TUI:** scrollable, sortable, filterable host table with per-host loss % and a latency sparkline.
- **Saved monitor lists:** save the current host set to a simple `.ml.txt` file and load it later.
- **Single static binary:** no libc, Python, or install step. Runs on old distros (e.g. EL8) as well as current ones.

## Installation

### Build from source

Building needs Go 1.26 or newer. The resulting binaries need nothing.

```bash
git clone git@github.com:longlanky/mincmon.git
cd mincmon
./build.sh                  # all platforms → dist/
./build.sh linux/amd64      # or just the ones you need
./build.sh linux/amd64/v2   # amd64 at a specific microarchitecture level
```

`build.sh` cross-compiles static binaries for Linux, Windows, and macOS (amd64 and arm64) into `dist/`:

```
dist/mincmon-linux-amd64-v1     dist/mincmon-windows-amd64.exe   dist/mincmon-darwin-amd64
dist/mincmon-linux-amd64-v2     dist/mincmon-windows-arm64.exe   dist/mincmon-darwin-arm64
dist/mincmon-linux-arm64
```

**Which Linux amd64 build?** `-v1` runs on any 64-bit x86 CPU. `-v2` needs SSE4.2/POPCNT, which is roughly any Intel CPU since Nehalem (2008) or AMD since Bulldozer (2011). mincmon spends its time waiting on the network, so the v2 build isn't noticeably faster: use `-v1` unless you have a reason not to. The Windows and macOS amd64 builds are v1.

`build.sh` always sets the amd64 level explicitly (default v1). Some Go toolchains default to the build machine's own level, and their binaries won't start on older CPUs.

Copy the one you need anywhere on your `PATH`. To run straight from the source tree instead, use `go run ./cmd/mincmon`.

### Permissions (Linux)

mincmon tries the following ways to send pings, in order, and uses the first that works:

| Backend | Requirements |
|---|---|
| **ICMP socket** (preferred) | Linux: your group must be inside `net.ipv4.ping_group_range`. Most current distros allow everyone by default. macOS: works out of the box. |
| **Windows ICMP API** | Windows: works out of the box, no administrator rights needed. |
| **Raw ICMP socket** | root, or the `CAP_NET_RAW` capability. |
| **System `ping` binary** | Last resort. It works anywhere `ping` exists, but starts a process per probe. |

The active backend is shown in the title bar. If mincmon falls back to the `ping` binary on Linux, it shows a hint. To enable the fast path, run one of these:

```bash
# allow unprivileged ICMP sockets for all users (persist it in /etc/sysctl.d/)
sudo sysctl -w net.ipv4.ping_group_range='0 2147483647'

# or grant just this binary raw-socket access
sudo setcap cap_net_raw+ep /path/to/mincmon
```

## Usage

```
mincmon [flags] [IP|CIDR|domain ...]
```

Run with no arguments to be asked interactively. Type the targets, or `load` to pick a saved list:

```
$ mincmon
mincmon v4.0  (probe: ICMP socket)

Input IP addresses, subnets, and/or domains to monitor, or enter 'load' to load an existing save:
> 192.0.2.10, 10.0.0.0/24, example.com
```

Or pass targets and options on the command line:

```bash
mincmon 192.0.2.10 2001:db8::10 example.com
mincmon -f infra.ml.txt                         # load a saved list
mincmon -i 1s -t 500ms 10.0.0.0/24              # faster probing
mincmon --resolver 1.1.1.1 --records A example.com
```

Flags must come before the targets.

| Flag | Default | Description |
|---|---|---|
| `-f file` | | Load hosts from a `.ml.txt` monitor list (can be combined with targets). |
| `-i interval` | `2s` | Time between probes, per host. |
| `-t timeout` | `1s` | How long to wait for a reply. |
| `--resolver IPs` | ask | DNS server(s) for domains, comma separated, or `system`. |
| `--records types` | ask | Record types to resolve: `A`, `AAAA`, or `both`. |
| `--backend name` | `auto` | Force a probe backend: `auto`, `icmp`, `raw`, or `exec`. |
| `--version` | | Print the version and exit. |

### Targets

Targets are comma separated, both on the command line and in the Add prompt:

| Input | Meaning |
|---|---|
| `192.0.2.10`, `2001:db8::1` | A single host. |
| `10.0.0.0/24`, `10.0.0.0/255.255.255.0`, `2001:db8::/64` | A subnet. mincmon asks you to refine it (see below). |
| `example.com` | A domain. mincmon asks which resolver and record types to use. |

### Subnet refinement

For each subnet, you can list the host offsets to monitor instead of the whole range. Offsets count from the network address:

```
Refine 10.0.0.0/24 (decimal offsets/ranges, e.g. 1,10-20; blank = first 1024 hosts): 1,10-20,254
```

- **IPv4** offsets are decimal: `1,10-20,254` gives `10.0.0.1`, `10.0.0.10`–`.20`, `10.0.0.254`.
- **IPv6** offsets are hex, optionally `:`-grouped: `1,a-f,1:0` under `2001:db8::/64` gives `::1`, `::a`–`::f`, `::1:0`.
- **Blank** means every usable host (network and broadcast are skipped for IPv4), capped at 1024. For subnets bigger than a /24 you'll be asked to confirm.

Any single subnet or refinement is capped at 1024 hosts.

## Keys

| Key | Action |
|---|---|
| `↑` `↓` / `j` `k` | Move the selection. |
| `PgUp` `PgDn` / `Space` | Page through the table. |
| `Home` `End` / `g` `G` | Jump to the first or last host. |
| `a` | Add targets (same prompts as at startup). |
| `r` | Remove hosts by IP, subnet, or domain label. Leave blank to remove the selected host. |
| `d` / `Delete` | Remove the selected host immediately. |
| `s` | Save the current hosts to a `.ml.txt` file. |
| `l` | Load a `.ml.txt` file from the current directory, or by path. |
| `o` / `O` | Cycle the sort column (added, IP, domain, state, latency, loss) / reverse it. |
| `/` | Filter by IP, domain, or state. `Esc` clears the filter. |
| `q` / `e` / `Ctrl+C` | Quit. |

In a prompt, `Enter` submits and `Esc` cancels.

### Columns

- **STATUS:** `● UP`, `○ DOWN`, or `◌ PENDING` (not probed yet).
- **LATENCY:** round-trip time of the last successful probe.
- **LOSS:** share of lost probes among the last 60.
- **UP / DOWN:** total successful and failed probes since the host was added.
- **HISTORY:** recent round-trip times, newest on the right, scaled to each host's own peak. `×` marks a lost probe.

The line under the table shows the selected host's state and when that state began, plus the last probe error if there was one other than a plain timeout.

## Monitor list files

Saved lists are plain text with the `.ml.txt` extension. Each line holds one host: the IP, a comma, and an optional domain label.

```
192.0.2.10,web.example.com
192.0.2.12,
2001:db8::10,web.example.com, www.example.com
```

Lines are split on the **first** comma only. If one IP was resolved from several domains, its label lists them all (`web.example.com, www.example.com`). Lines with an invalid IP are skipped.

The repo's `.gitignore` excludes `*.ml.txt`, because saved lists tend to describe private infrastructure.

## Development

```bash
go test ./...          # the DNS and probe tests use the network and skip if it's unavailable
go vet ./... && GOOS=windows go vet ./... && GOOS=darwin go vet ./...
```

| Package | Responsibility |
|---|---|
| `cmd/mincmon` | Flags, startup prompts, launching the TUI. |
| `internal/targets` | Input classification, refinement parsing, subnet expansion, de-duplication. |
| `internal/probe` | Probe backends and their automatic selection. |
| `internal/monitor` | One goroutine per host, and the shared status table. |
| `internal/resolve` | A/AAAA lookups via custom DNS servers or the system resolver. |
| `internal/store` | `.ml.txt` load/save. |
| `internal/app` | Interactive add/remove/save/load flows, shared by startup and the TUI. |
| `internal/tui` | The Bubble Tea interface. |

### Platform notes

- **Windows:** the Windows ICMP API backend builds and passes vet, but it has not been run on real Windows yet. Reports welcome.
- **macOS:** builds only; it has not been run on a Mac yet.
