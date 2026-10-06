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

// Load reads a monitor list. Rows are split on the FIRST comma only:
// merged labels like "a.com, b.com" live entirely in the domain field.
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
		ipS, domain, _ := strings.Cut(line, ",")
		ip, err := netip.ParseAddr(strings.TrimSpace(ipS))
		if err != nil {
			skipped++
			continue
		}
		items = append(items, targets.Target{Addr: ip, Domain: strings.TrimSpace(domain)})
	}
	return items, skipped, sc.Err()
}

// Save writes a monitor list atomically (temp file + rename) and returns
// the number of rows written. An empty list writes an empty file.
func Save(path string, items []targets.Target) (int, error) {
	var b strings.Builder
	for _, t := range items {
		b.WriteString(t.Addr.String())
		b.WriteByte(',')
		b.WriteString(t.Domain)
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
