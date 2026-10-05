// Package store reads and writes .ml.txt monitor lists: one "ip,domain"
// row per host, domain blank if none.
package store

import (
	"bufio"
	"net/netip"
	"os"
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
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []targets.Target
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ipS, domain, _ := strings.Cut(line, ",")
		ip, err := netip.ParseAddr(strings.TrimSpace(ipS))
		if err != nil {
			continue
		}
		items = append(items, targets.Target{Addr: ip, Domain: strings.TrimSpace(domain)})
	}
	return items, sc.Err()
}

// Save writes a monitor list and returns the number of rows written.
func Save(path string, items []targets.Target) (int, error) {
	var b strings.Builder
	for _, t := range items {
		b.WriteString(t.Addr.String())
		b.WriteByte(',')
		b.WriteString(t.Domain)
		b.WriteByte('\n')
	}
	if len(items) == 0 {
		b.WriteByte('\n')
	}
	return len(items), os.WriteFile(path, []byte(b.String()), 0o644)
}

// List returns the .ml.txt files in the current directory, sorted.
func List() []string {
	ents, _ := os.ReadDir(".")
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), Ext) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
