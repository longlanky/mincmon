// Package store reads and writes .ml.txt monitor lists: one "ip,domain"
// row per host, domain blank if none.
package store

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mincmon/internal/targets"
)

// Ext is the monitor-list file extension.
const Ext = ".ml.txt"

// Load reads a monitor list. Rows are "ip,domain" or "ip,domain,resolver":
// the domain field is split on the FIRST comma only, because merged labels
// like "a.com, b.com" live entirely in the domain field. An optional third
// field records which DNS resolver produced the entry; it is detected by
// checking whether the segment after the last comma parses as an IP
// address (a domain label can never be an IP, so this is unambiguous).
// Rows with an invalid IP are skipped.
func Load(path string) ([]targets.Target, error) {
	items, _, err := LoadWithStats(path)
	return items, err
}

// LoadWithStats also reports how many non-blank rows were skipped due to
// an invalid IP, so callers can warn instead of silently dropping hosts.
func LoadWithStats(path string) (items []targets.Target, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// Allow long lines (merged labels can be wide).
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		ipS, rest, _ := strings.Cut(line, ",")
		ip, err := netip.ParseAddr(strings.TrimSpace(ipS))
		if err != nil {
			skipped++
			continue
		}
		// Optional third field: an IP after the last comma is the
		// resolver that produced this entry (domains can't be IPs, so
		// merged labels like "a.com, b.com" never trigger this).
		if r, ok := parseTrailingResolver(rest); ok {
			d := strings.TrimSpace(rest[:strings.LastIndex(rest, ",")])
			items = append(items, targets.Target{Addr: ip, Domain: d, Resolver: r})
			continue
		}
		items = append(items, targets.Target{Addr: ip, Domain: strings.TrimSpace(rest)})
	}
	return items, skipped, sc.Err()
}

// parseTrailingResolver reports the IP after the last comma of s, if any.
func parseTrailingResolver(s string) (netip.Addr, bool) {
	idx := strings.LastIndex(s, ",")
	if idx < 0 {
		return netip.Addr{}, false
	}
	r, err := netip.ParseAddr(strings.TrimSpace(s[idx+1:]))
	if err != nil {
		return netip.Addr{}, false
	}
	return r.WithZone(""), true
}

// Save writes a monitor list atomically (temp file + rename) and returns
// the number of rows written. An empty list writes an empty file. Rows
// with a recorded resolver get an optional third field (ip,domain,resolver);
// system-resolved rows stay two-field, so old files round-trip unchanged.
func Save(path string, items []targets.Target) (int, error) {
	var b strings.Builder
	for _, t := range items {
		b.WriteString(t.Addr.String())
		b.WriteByte(',')
		b.WriteString(t.Domain)
		if t.Resolver.IsValid() {
			b.WriteByte(',')
			b.WriteString(t.Resolver.String())
		}
		b.WriteByte('\n')
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".mltmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	// Best effort cleanup on failure; success renames away.
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, fmt.Errorf("rename temp save file: %w", err)
	}
	return len(items), nil
}

// List returns the .ml.txt files in the current directory, sorted.
// Unreadable directories yield an empty list.
func List() []string {
	ents, err := os.ReadDir(".")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), Ext) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
