// Package app holds the interactive flows (add, remove, save, load) shared
// by the startup prompts and the TUI. Each flow talks to the user only
// through a Prompter.
package app

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"mincmon/internal/monitor"
	"mincmon/internal/resolve"
	"mincmon/internal/store"
	"mincmon/internal/targets"
)

// ErrCancelled is returned by a Prompter when the user aborts (Esc/EOF).
var ErrCancelled = errors.New("cancelled")

// Prompter asks the user for a line of input or shows a note.
type Prompter interface {
	Prompt(text string) (string, error)
	Notify(text string)
}

// DNSOptions pre-answers the resolver / record-type prompts (from CLI
// flags). Unset fields are asked interactively.
type DNSOptions struct {
	Resolvers    []netip.Addr
	ResolversSet bool
	WantA        bool
	WantAAAA     bool
	RecordsSet   bool
}

// ResolveEntries expands networks (asking for refinements of non-single
// subnets) and resolves domains (asking for resolvers and record types).
// The result is not yet de-duplicated.
func ResolveEntries(entries []targets.Entry, p Prompter, opt DNSOptions) ([]targets.Target, error) {
	var out []targets.Target
	var domains []string
	warn := func(s string) { p.Notify(s) }

	for _, e := range entries {
		if e.Kind == targets.Domain {
			if targets.LooksLikeNetwork(e.Name) {
				p.Notify(fmt.Sprintf("! %q looks like a subnet but didn't parse; treating as a domain.", e.Name))
			}
			domains = append(domains, e.Name)
			continue
		}
		if e.Single {
			out = append(out, targets.Target{Addr: e.Addr})
			continue
		}
		v6 := e.Prefix.Addr().Is6()
		format := "decimal offsets/ranges, e.g. 1,10-20"
		if v6 {
			format = "hex offsets/ranges, e.g. 1,a-f,0:10"
		}
		var offsets []targets.Offset
		var refined bool
		for {
			raw, err := p.Prompt(fmt.Sprintf("Refine %s (%s; blank = first %d hosts): ",
				e.Prefix, format, targets.HostCap))
			if err != nil {
				return nil, err
			}
			offsets, refined, err = targets.ParseRefine(raw, v6, targets.HostCap)
			if err != nil {
				p.Notify("! " + err.Error())
				continue
			}
			if !refined && e.Prefix.Addr().BitLen()-e.Prefix.Bits() > 8 { // > 256 addresses
				ans, err := p.Prompt(fmt.Sprintf("Monitor the first %d hosts of %s? (y/N): ",
					targets.HostCount(e.Prefix, targets.HostCap), e.Prefix))
				if err != nil {
					return nil, err
				}
				if !strings.EqualFold(strings.TrimSpace(ans), "y") {
					continue
				}
			}
			break
		}
		for _, a := range targets.Expand(e, offsets, refined, targets.HostCap, warn) {
			out = append(out, targets.Target{Addr: a})
		}
	}

	if len(domains) == 0 {
		return out, nil
	}
	resolvers := opt.Resolvers
	if !opt.ResolversSet {
		raw, err := p.Prompt("Domains detected: DNS resolver IP(s), comma separated, " +
			"or blank for the system resolver: ")
		if err != nil {
			return nil, err
		}
		resolvers = nil
		for _, r := range strings.Split(raw, ",") {
			if r = strings.TrimSpace(r); r == "" {
				continue
			}
			if a, err := netip.ParseAddr(r); err == nil {
				resolvers = append(resolvers, a.WithZone(""))
			} else {
				p.Notify(fmt.Sprintf("! ignoring invalid resolver %q", r))
			}
		}
	}
	wantA, wantAAAA := opt.WantA, opt.WantAAAA
	if !opt.RecordsSet {
		raw, err := p.Prompt("Resolve A records, AAAA records, or leave blank for both: ")
		if err != nil {
			return nil, err
		}
		wantA, wantAAAA = ParseRecords(raw)
	}
	for _, d := range domains {
		results := resolve.DomainDetailed(d, resolvers, wantA, wantAAAA)
		if len(results) == 0 {
			p.Notify(fmt.Sprintf("! No records resolved for %s.", d))
		}
		for _, r := range results {
			out = append(out, targets.Target{Addr: r.Addr, Domain: d, Resolver: r.Resolver})
		}
	}
	return out, nil
}

// ParseRecords maps "A", "AAAA", "both"/blank to record types. Invalid
// input falls back to "both" so interactive users are never blocked on a
// typo (the strict variant reports it instead).
func ParseRecords(s string) (wantA, wantAAAA bool) {
	wantA, wantAAAA, _ = ParseRecordsStrict(s)
	return wantA, wantAAAA
}

// ParseRecordsStrict is like ParseRecords but reports invalid input.
func ParseRecordsStrict(s string) (wantA, wantAAAA bool, err error) {
	switch t := strings.ToUpper(strings.TrimSpace(s)); t {
	case "A":
		return true, false, nil
	case "AAAA":
		return false, true, nil
	case "", "BOTH", "A,AAAA", "AAAA,A":
		return true, true, nil
	default:
		return false, false, fmt.Errorf("invalid record types %q: want A, AAAA or both", s)
	}
}

// Add asks for targets and starts monitoring them.
func Add(m *monitor.Monitor, p Prompter, opt DNSOptions) string {
	raw, err := p.Prompt("Add IP(s)/range(s)/domain(s) (comma separated): ")
	if err != nil || strings.TrimSpace(raw) == "" {
		return "Add cancelled."
	}
	entries := targets.ParseEntries(raw)
	if len(entries) == 0 {
		return "No valid entries."
	}
	ts, err := ResolveEntries(entries, p, opt)
	if err != nil {
		return "Add cancelled."
	}
	added := m.Add(targets.Dedupe(ts))
	if len(added) == 0 {
		if len(ts) == 0 {
			return "Nothing matched (refinement was empty or out of range)."
		}
		return "Nothing new added."
	}
	return fmt.Sprintf("Added %d host(s).", len(added))
}

// Remove asks which hosts to drop. Blank input removes selected (if valid).
func Remove(m *monitor.Monitor, p Prompter, selected netip.Addr) string {
	q := "Remove IP(s)/range(s)/domain(s) (comma separated): "
	if selected.IsValid() {
		q = fmt.Sprintf("Remove IP(s)/range(s)/domain(s) (comma separated; blank = %s): ", selected)
	}
	raw, err := p.Prompt(q)
	if err != nil {
		return "Remove cancelled."
	}
	if strings.TrimSpace(raw) == "" {
		if !selected.IsValid() {
			return "Remove cancelled."
		}
		raw = selected.String()
	}
	return RemoveMatching(m, raw)
}

// normAddr strips zones from IPv4 and unmaps 4-in-6 so comparisons work
// regardless of how the address was typed.
func normAddr(a netip.Addr) netip.Addr {
	if a.Is4In6() {
		a = a.Unmap()
	}
	if a.Is4() {
		return a.WithZone("")
	}
	return a.WithZone("")
}

// RemoveMatching removes hosts inside any listed network, or labelled with
// any listed domain.
func RemoveMatching(m *monitor.Monitor, raw string) string {
	var drop []netip.Addr
	current := m.Targets()
	for _, e := range targets.ParseEntries(raw) {
		for _, t := range current {
			switch {
			case e.Kind == targets.Domain:
				if t.Domain != "" && targets.HasLabel(t.Domain, e.Name) {
					drop = append(drop, t.Addr)
				}
			case e.Single:
				if normAddr(t.Addr) == normAddr(e.Addr) {
					drop = append(drop, t.Addr)
				}
			case e.Prefix.Contains(normAddr(t.Addr)):
				drop = append(drop, t.Addr)
			}
		}
	}
	removed := m.Remove(drop)
	if len(removed) == 0 {
		return "Nothing matched."
	}
	return fmt.Sprintf("Removed %d host(s).", len(removed))
}

// Save asks for a filename and writes the monitor list.
func Save(m *monitor.Monitor, p Prompter) string {
	name, err := p.Prompt(fmt.Sprintf("Enter filename (extension '%s' auto-appended if missing): ", store.Ext))
	name = strings.TrimSpace(name)
	if err != nil || name == "" {
		return "Save cancelled."
	}
	path := name
	if !strings.HasSuffix(name, store.Ext) {
		path = name + store.Ext
	}
	if _, err := os.Stat(path); err == nil {
		ans, err := p.Prompt(fmt.Sprintf("%s exists. Overwrite? (y/N): ", path))
		if err != nil || !strings.EqualFold(strings.TrimSpace(ans), "y") {
			return "Save cancelled."
		}
	}
	n, err := store.Save(path, m.Targets())
	if err != nil {
		return fmt.Sprintf("Save failed: %v", err)
	}
	return fmt.Sprintf("Saved %d host(s) to %s", n, path)
}

// PromptLoad picks a .ml.txt file (numbered list, name, or path) and loads it.
func PromptLoad(p Prompter) ([]targets.Target, string) {
	files := store.List()
	byNum := map[string]string{}
	var b strings.Builder
	if len(files) > 0 {
		b.WriteString("Available monitor lists:\n")
		for i, f := range files {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, f)
			byNum[strconv.Itoa(i+1)] = f
		}
	} else {
		fmt.Fprintf(&b, "(No %s files in current directory.)\n", store.Ext)
	}
	b.WriteString("Select a number, or enter a file name/path: ")
	sel, err := p.Prompt(b.String())
	sel = strings.TrimSpace(sel)
	if err != nil || sel == "" {
		return nil, "Load cancelled."
	}
	path := sel
	// A literal file takes precedence over a number, so files named
	// "1" or "123.ml.txt" still load. Only fall back to the numbered
	// list when no such file exists.
	if _, statErr := os.Stat(sel); statErr != nil {
		if f, ok := byNum[sel]; ok {
			path = f
		} else if n, convErr := strconv.Atoi(sel); convErr == nil && n >= 1 && n <= len(files) {
			path = files[n-1]
		}
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return nil, fmt.Sprintf("File not found: %s", path)
	}
	items, skipped, err := store.LoadWithStats(path)
	if err != nil {
		return nil, fmt.Sprintf("Load failed: %v", err)
	}
	msg := fmt.Sprintf("Loaded %d host(s) from %s", len(items), filepath.Base(path))
	if skipped > 0 {
		msg += fmt.Sprintf(" (%d bad row(s) skipped)", skipped)
	}
	if len(items) == 0 {
		if skipped == 0 {
			msg = fmt.Sprintf("No hosts in %s", filepath.Base(path))
		}
		return nil, msg
	}
	return targets.Dedupe(items), msg
}

// Load asks for a monitor list and adds its hosts.
func Load(m *monitor.Monitor, p Prompter) string {
	items, msg := PromptLoad(p)
	if len(items) == 0 {
		return msg
	}
	added := m.Add(targets.Dedupe(items))
	if len(added) == 0 {
		return fmt.Sprintf("%s (0 new - already monitored).", msg)
	}
	return fmt.Sprintf("%s (%d new).", msg, len(added))
}

// Reresolve re-resolves selected domain labels currently monitored and
// adds any new addresses (existing hosts keep their state and history).
// The user picks which domains (blank = all) and the resolver: blank uses
// each domain's original resolver (recorded provenance), "system" forces
// the system resolver, or an explicit IP list overrides for this run.
func Reresolve(m *monitor.Monitor, p Prompter, opt DNSOptions) string {
	// label -> set of resolvers that produced it (empty set = system).
	provenance := map[string]map[netip.Addr]bool{}
	for _, t := range m.Targets() {
		for _, l := range strings.Split(t.Domain, ",") {
			if l = strings.TrimSpace(l); l != "" {
				if provenance[l] == nil {
					provenance[l] = map[netip.Addr]bool{}
				}
				if t.Resolver.IsValid() {
					provenance[l][t.Resolver] = true
				}
			}
		}
	}
	if len(provenance) == 0 {
		return "No domain-labelled hosts to re-resolve."
	}

	// Which domains?
	names := make([]string, 0, len(provenance))
	for l := range provenance {
		names = append(names, l)
	}
	sort.Strings(names)
	selected := names
	if !opt.ResolversSet { // domain picker only makes sense interactively
		raw, err := p.Prompt(fmt.Sprintf(
			"Re-resolve which domain(s)? (comma separated; blank = all %d): ", len(names)))
		if err != nil {
			return "Re-resolve cancelled."
		}
		if raw = strings.TrimSpace(raw); raw != "" {
			selected = nil
			for _, s := range strings.Split(raw, ",") {
				s = strings.TrimSpace(s)
				match := ""
				for _, n := range names {
					if strings.EqualFold(n, s) {
						match = n
						break
					}
				}
				if match == "" {
					p.Notify(fmt.Sprintf("! %q is not a monitored domain label; skipped.", s))
					continue
				}
				selected = append(selected, match)
			}
			if len(selected) == 0 {
				return "No monitored domains matched."
			}
		}
	}

	// Which resolver?
	var override []netip.Addr
	useOverride := false
	switch {
	case opt.ResolversSet:
		override, useOverride = opt.Resolvers, true
	default:
		raw, err := p.Prompt("Resolver: blank = each domain's original, \"system\", or IP(s) comma separated: ")
		if err != nil {
			return "Re-resolve cancelled."
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			// per-domain provenance (the default)
		} else if strings.EqualFold(raw, "system") {
			override, useOverride = nil, true
		} else {
			for _, r := range strings.Split(raw, ",") {
				if r = strings.TrimSpace(r); r == "" {
					continue
				}
				if a, err := netip.ParseAddr(r); err == nil {
					override = append(override, a.WithZone(""))
				} else {
					p.Notify(fmt.Sprintf("! ignoring invalid resolver %q", r))
				}
			}
			useOverride = true
		}
	}

	wantA, wantAAAA := opt.WantA, opt.WantAAAA
	if !opt.RecordsSet {
		raw, err := p.Prompt("Resolve A records, AAAA records, or leave blank for both: ")
		if err != nil {
			return "Re-resolve cancelled."
		}
		wantA, wantAAAA = ParseRecords(raw)
	}

	var ts []targets.Target
	changed := 0
	for _, d := range selected {
		resolvers := override
		if !useOverride {
			// Query every resolver that ever produced this label, so
			// DNS changes from any of them are seen.
			set := provenance[d]
			resolvers = make([]netip.Addr, 0, len(set))
			for r := range set {
				resolvers = append(resolvers, r)
			}
			sort.Slice(resolvers, func(i, j int) bool { return resolvers[i].Less(resolvers[j]) })
		}
		results := resolve.DomainDetailed(d, resolvers, wantA, wantAAAA)
		if len(results) == 0 {
			p.Notify(fmt.Sprintf("! No records resolved for %s.", d))
			continue
		}
		changed++
		for _, r := range results {
			ts = append(ts, targets.Target{Addr: r.Addr, Domain: d, Resolver: r.Resolver})
		}
	}
	added := m.Add(targets.Dedupe(ts))
	return fmt.Sprintf("Re-resolved %d/%d domain(s), %d new host(s).",
		changed, len(selected), len(added))
}
