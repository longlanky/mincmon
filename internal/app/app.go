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
				resolvers = append(resolvers, a)
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
		addrs := resolve.Domain(d, resolvers, wantA, wantAAAA)
		if len(addrs) == 0 {
			p.Notify(fmt.Sprintf("! No records resolved for %s.", d))
		}
		for _, a := range addrs {
			out = append(out, targets.Target{Addr: a, Domain: d})
		}
	}
	return out, nil
}

// ParseRecords maps "A", "AAAA" or anything else (both) to record types.
func ParseRecords(s string) (wantA, wantAAAA bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "A":
		return true, false
	case "AAAA":
		return false, true
	}
	return true, true
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
				if t.Addr == e.Addr || t.Addr.WithZone("") == e.Addr {
					drop = append(drop, t.Addr)
				}
			case e.Prefix.Contains(t.Addr.WithZone("")):
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
	name, err := p.Prompt(fmt.Sprintf("Enter filename (without extension; '%s' will be appended): ", store.Ext))
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
	var b strings.Builder
	if len(files) > 0 {
		b.WriteString("Available monitor lists:\n")
		for i, f := range files {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, f)
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
	if n, err := strconv.Atoi(sel); err == nil {
		if n < 1 || n > len(files) {
			return nil, fmt.Sprintf("Invalid selection: %s", sel)
		}
		path = files[n-1]
	}
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		return nil, fmt.Sprintf("File not found: %s", path)
	}
	items, err := store.Load(path)
	if err != nil {
		return nil, fmt.Sprintf("Load failed: %v", err)
	}
	return items, fmt.Sprintf("Loaded %d host(s) from %s", len(items), filepath.Base(path))
}

// Load asks for a monitor list and adds its hosts.
func Load(m *monitor.Monitor, p Prompter) string {
	items, msg := PromptLoad(p)
	if len(items) == 0 {
		return msg
	}
	added := m.Add(items)
	return fmt.Sprintf("%s (%d new).", msg, len(added))
}
