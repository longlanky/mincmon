# ✨ mincmon Changelog

## v1.1.0 — Stability & correctness pass 🛠️
*Previous release treated as `1.0.0`. This is a **minor bump to `1.1.0`**: lots of bug fixes + user-visible reliability wins, no major breaking changes.*

> End-user TL;DR: fewer silent surprises, clearer error messages, safer saves/loads, faster DNS, and no more accidental quits. Upgrade and keep your `.ml.txt` files — they're fully compatible. 🎉

### 🐛 Critical bugs fixed — you’ll notice these

* 🧵 **Overlapping prompts could freeze the TUI** — pressing `a / r / s / l` quickly used to start two flows at once, overwriting the prompt and leaking the first one. Now the UI correctly shows `busy` and blocks a second flow until the first finishes. Impact: no more stuck prompts. 🧊 → ✅
* 🔗 **Link-local zones no longer cross-talk** — `fe80::1%eth0` vs `%eth1` shared the same reply matcher, so replies could go to the wrong host. Now zones are matched correctly. Impact: link-local monitoring is trustworthy. 🏠
* ⚠️ **Refinements outside the subnet now warn** — `10.0.0.0/24` + `300` used to silently give you 5 hosts with no message, and a fully out-of-range refinement gave 0 hosts with silence. Now you see `! N refined offset(s) outside … skipped` and `! matched no hosts`. Impact: typos don’t silently drop hosts. 🔍
* 🏷️ **Domain labels now merge on re-add** — adding `1.1.1.1` from `a.com` then again from `b.com` used to keep only `a.com`. Now it becomes `a.com, b.com`, same as initial dedupe. Impact: multi-domain IPs keep all names. 📛
* 📂 **Loading a list dedupes + reports problems** — TUI Load used to skip dedupe, silently drop bad rows, and confuse files named `1`. Now it dedupes, says `Loaded 38 host(s) from infra.ml.txt (2 bad row(s) skipped)`, says `No hosts in …` for empty files, and a real file always wins over a number. Impact: loads are predictable. 📥
* 🔍 **Remove by IP handles zones / 4-in-6** — `RemoveMatching` compared zones asymmetrically, so `::ffff:10.0.0.1` or `fe80::1%eth0` might not match. Now both sides are normalised. Impact: `r` / `d` actually removes what you typed. 🗑️
* 🏷️ **Labels tolerate `a.com,b.com`** — hand-edited lists without a space after the comma used to duplicate to `a.com,b.com, b.com`. Now `,` + trim is used. Impact: hand edits don’t snowball. ✏️

### ⚡ Reliability & performance — quiet but important

* 💓 **Steady probe rhythm** — workers used `probe + sleep(Interval)`, so effective period was `Interval + probe_time`. Now immediate first probe + `ticker(Interval)`. Impact: `2s` really means `2s`. ⏱️
* 🧹 **Clean shutdown** — `Stop()` only cancelled; now it `Wait()`s for workers. Plus SIGINT/SIGTERM handling in `main`. Impact: `q` / Ctrl-C doesn’t leave ping processes hanging. 👋
* 🧠 **History memory capped** — per-host history reslice retained a growing backing array. Now copies to a fixed 60-entry array. Impact: long runs with 1000 hosts stay lean. 📉
* 💥 **Exec backend capped at 32 concurrent pings** — 1000 hosts on the `ping binary` fallback used to fork-bomb. Now a semaphore queues them. Impact: fallback mode is safe on big lists. 🐧
* 🌐 **DNS is parallel + follows CNAMEs** — custom-resolver lookups were sequential (`types × resolvers × 3s`) and dropped CNAME-only answers (common with CDNs). Now parallel per-type/per-server + up to 8 CNAME hops. Impact: `example.com` behind a CDN resolves, and multi-resolver setups are ~Nx faster. 🚀
* 💾 **Atomic saves** — `Save` truncated in place; crash = corrupt list. Now temp-file + rename. Empty list = empty file (was `\n`). `#` comment lines allowed and skipped. Impact: your saves survive crashes. 🛡️
* 🔌 **Backend close errors no longer swallowed** — `dual.Close()` now `errors.Join`s v4/v6 errors. Impact: shutdown issues surface in logs. 🔎

### 🎨 UX polish — small changes, less confusion

* ⌨️ **`e` no longer quits** — `e` was an accidental-quit trap (e.g. typing fast near `r`/`q`). Now only `q` / `Ctrl+C`. Impact: fewer “oops I quit” moments. 🙈 → 😌
* 📊 **Loss sort is now ascending like the rest** — was descending (worst first) while latency was ascending. Now `o` cycles consistently; use `O` to reverse. Impact: sorting is predictable. 🔢
* 💬 **Clearer messages** — `Add` says `Nothing matched (refinement was empty…)` when appropriate, `Load` says `(0 new - already monitored)`, `Save` prompt says `extension auto-appended if missing`, subnet typos like `10.0.0.0/33` warn `looks like a subnet… treating as domain`. Impact: less guessing. 💡
* 🖥️ **CLI fail-fast** — `mincmon 10.0.0.1 -i 1s` used to DNS-lookup `-i`. Now errors `flags must come before targets`. `--records foo` now errors instead of silently using `both`. `--resolver 1.1.1.1,` trailing comma tolerated. `-f` bad rows reported. `fail()` now returns exit codes without skipping `Close()`. Impact: copy-paste flag mistakes are obvious. 🚦
* 🪟 **Windows: honest zone errors + cancellable probes** — unknown `%zone` used to silently probe scope 0 (timeout). Now `unknown zone "foo"` errors. Blocking `IcmpSendEcho` now runs in a goroutine with `ctx` select, so quit doesn’t hang. Reply bounds-checked. Impact: Windows link-local actually tells you what’s wrong. 🪟

### 🔧 For contributors / tech notes

* `HostCount` / `Expand` no longer rely on 32-bit `int` shift tricks or `1<<64` wrap — explicit `uint64(1)` paths. 🧮
* `ParseRefine` negative-remaining guard (`cap-len < 0`) avoids `uint64` wrap. 🛡️
* `Remove` order filter allocates new slice instead of `order[:0]` in-place trick. 📋
* `TUI refresh()` allocates new rows slice; `promptReq` copies loop var + guards double-prompt. 🧵
* `exec` normalises `4-in-6 → 4` but preserves v6 zones; `socket receive` does zone-aware fallback matching. 🔗
* Tests: `TestExpand` updated for new warn-on-skip; `go vet ./... + windows + darwin` clean; `go test ./...` green. ✅

---
*Upgrade: replace binary (`./build.sh`), keep `.ml.txt`. No migration needed.*
