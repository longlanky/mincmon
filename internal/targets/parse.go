// Package targets classifies user input (IPs, subnets, domains), expands
// subnets into host lists, and de-duplicates resolved targets.
package targets

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// HostCap is the maximum number of hosts expanded from any single subnet
// or refinement.
const HostCap = 1024

// Kind distinguishes network entries from domain names.
type Kind int

const (
	Network Kind = iota
	Domain
)

// Entry is one classified input token.
type Entry struct {
	Kind   Kind
	Prefix netip.Prefix // masked network (Network entries)
	Single bool         // Network entry that is exactly one host (/32, /128)
	Addr   netip.Addr   // the host address when Single (keeps any IPv6 zone)
	Name   string       // domain name (Domain entries)
}

// Target is a monitored host and its (possibly merged) domain label.
type Target struct {
	Addr   netip.Addr
	Domain string
}

// Classify turns one input token into a network or domain entry. Anything
// that does not parse as an IP address or CIDR is treated as a domain.
func Classify(part string) Entry {
	part = strings.TrimSpace(part)
	if strings.Contains(part, "/") {
		if p, ok := parsePrefix(part); ok {
			e := Entry{Kind: Network, Prefix: p.Masked()}
			if p.Bits() == p.Addr().BitLen() {
				e.Single = true
				e.Addr = p.Addr()
			}
			return e
		}
		return Entry{Kind: Domain, Name: part}
	}
	if a, err := netip.ParseAddr(part); err == nil {
		return Entry{
			Kind:   Network,
			Prefix: netip.PrefixFrom(a.WithZone(""), a.BitLen()),
			Single: true,
			Addr:   a,
		}
	}
	return Entry{Kind: Domain, Name: part}
}

// parsePrefix accepts "addr/len" and IPv4 "addr/dotted.netmask", with host
// bits allowed (they are masked off by the caller).
func parsePrefix(s string) (netip.Prefix, bool) {
	if p, err := netip.ParsePrefix(s); err == nil {
		return p, true
	}
	addrS, maskS, _ := strings.Cut(s, "/")
	addr, err1 := netip.ParseAddr(addrS)
	mask, err2 := netip.ParseAddr(maskS)
	if err1 != nil || err2 != nil || !addr.Is4() || !mask.Is4() {
		return netip.Prefix{}, false
	}
	m := addrToU128(mask).lo
	ones := 0
	for m&(1<<31) != 0 {
		ones++
		m = (m << 1) & 0xffffffff
	}
	if m != 0 { // non-contiguous mask
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, ones), true
}

// ParseEntries splits comma-separated input into classified entries,
// preserving order and skipping blanks.
func ParseEntries(text string) []Entry {
	var entries []Entry
	for _, part := range strings.Split(text, ",") {
		if part = strings.TrimSpace(part); part != "" {
			entries = append(entries, Classify(part))
		}
	}
	return entries
}

// Offset is a host offset from a network's base address.
type Offset = u128

func parseOffset(token string, v6 bool) (Offset, error) {
	token = strings.TrimSpace(token)
	bad := fmt.Errorf("bad offset: %q", token)
	if !v6 {
		n, err := strconv.ParseUint(token, 10, 64)
		if err != nil {
			return Offset{}, bad
		}
		return Offset{0, n}, nil
	}
	if strings.Contains(token, ":") {
		groups := strings.Split(token, ":")
		if len(groups) > 8 {
			return Offset{}, bad
		}
		var v Offset
		for _, g := range groups {
			g = strings.TrimSpace(g)
			var n uint64
			if g != "" {
				var err error
				if n, err = strconv.ParseUint(g, 16, 16); err != nil {
					return Offset{}, bad
				}
			}
			v, _ = v.mulAdd(1<<16, n) // ≤8 groups of 16 bits cannot overflow
		}
		return v, nil
	}
	t := strings.TrimPrefix(strings.ToLower(token), "0x")
	if t == "" {
		return Offset{}, bad
	}
	var v Offset
	for _, c := range t {
		d, err := strconv.ParseUint(string(c), 16, 8)
		if err != nil {
			return Offset{}, bad
		}
		var over bool
		if v, over = v.mulAdd(16, d); over {
			return Offset{}, bad
		}
	}
	return v, nil
}

// ParseRefine parses a refinement like "1,10-20" (IPv4, decimal) or
// "1,a-f,0:10" (IPv6, hex or ':'-grouped hex). It returns refined=false for
// blank input, meaning "no refinement".
func ParseRefine(text string, v6 bool, cap int) (offsets []Offset, refined bool, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, false, nil
	}
	tooMany := fmt.Errorf("refinement expands to more than %d hosts", cap)
	offsets = []Offset{}
	for _, part := range strings.Split(text, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if loS, hiS, isRange := strings.Cut(part, "-"); isRange {
			lo, err := parseOffset(loS, v6)
			if err != nil {
				return nil, true, err
			}
			hi, err := parseOffset(hiS, v6)
			if err != nil {
				return nil, true, err
			}
			if hi.cmp(lo) < 0 {
				lo, hi = hi, lo
			}
			// Check the span BEFORE expanding: a range like 0-ffffffff would
			// otherwise allocate billions of offsets.
			span := hi.sub(lo)
			if span.cmp(Offset{0, uint64(cap - len(offsets))}) >= 0 {
				return nil, true, tooMany
			}
			for o := lo; ; o, _ = o.add(Offset{0, 1}) {
				offsets = append(offsets, o)
				if o == hi {
					break
				}
			}
		} else {
			o, err := parseOffset(part, v6)
			if err != nil {
				return nil, true, err
			}
			offsets = append(offsets, o)
		}
		if len(offsets) > cap {
			return nil, true, tooMany
		}
	}
	return offsets, true, nil
}

// Expand returns the hosts to monitor for a network entry. Without a
// refinement it yields the usable hosts (as Python's ipaddress hosts());
// with one it yields base+offset for each offset inside the prefix.
// Truncation to cap is reported through warn.
func Expand(e Entry, offsets []Offset, refined bool, cap int, warn func(string)) []netip.Addr {
	if e.Single {
		return []netip.Addr{e.Addr}
	}
	p := e.Prefix
	base := addrToU128(p.Addr())
	var out []netip.Addr

	if refined {
		for _, off := range offsets {
			if len(out) >= cap {
				warn(fmt.Sprintf("! Refinement for %s truncated to %d hosts.", p, cap))
				break
			}
			v, over := base.add(off)
			if over {
				continue
			}
			a, ok := u128ToAddr(v, p.Addr())
			if ok && p.Contains(a) {
				out = append(out, a)
			}
		}
		return out
	}

	hostBits := p.Addr().BitLen() - p.Bits()
	last := base
	if hostBits >= 64 {
		last.lo = ^uint64(0)
		last.hi |= (1 << (hostBits - 64)) - 1 // 1<<64 wraps to 0 → all ones
	} else {
		last.lo |= (1 << hostBits) - 1
	}
	start, end := base, last
	if p.Addr().Is4() && hostBits > 1 {
		// IPv4: skip network and broadcast (except /31)
		start, _ = base.add(Offset{0, 1})
		end = last.sub(Offset{0, 1})
	} else if p.Addr().Is6() && hostBits > 1 {
		// IPv6: skip the Subnet-Router anycast address (except /127)
		start, _ = base.add(Offset{0, 1})
	}
	for v := start; v.cmp(end) <= 0; v, _ = v.add(Offset{0, 1}) {
		if len(out) >= cap {
			warn(fmt.Sprintf("! Subnet %s truncated to first %d hosts.", p, cap))
			break
		}
		a, _ := u128ToAddr(v, p.Addr())
		out = append(out, a)
		if v == end { // avoid wrap-around at the top of the address space
			break
		}
	}
	return out
}

// HostCount returns min(number of addresses in p, limit).
func HostCount(p netip.Prefix, limit int) int {
	hostBits := p.Addr().BitLen() - p.Bits()
	if hostBits >= 31 || 1<<hostBits > limit {
		return limit
	}
	return 1 << hostBits
}

// Dedupe de-duplicates by address, keeping first-seen order. When one
// address was resolved from several domains their labels are merged as
// "a.com, b.com".
func Dedupe(in []Target) []Target {
	idx := map[netip.Addr]int{}
	var out []Target
	for _, t := range in {
		i, seen := idx[t.Addr]
		if !seen {
			idx[t.Addr] = len(out)
			out = append(out, t)
			continue
		}
		cur := out[i].Domain
		switch {
		case t.Domain == "" || t.Domain == cur:
		case cur == "":
			out[i].Domain = t.Domain
		default:
			if !HasLabel(cur, t.Domain) {
				out[i].Domain = cur + ", " + t.Domain
			}
		}
	}
	return out
}

// HasLabel reports whether a (possibly merged) domain label contains name.
func HasLabel(label, name string) bool {
	for _, l := range strings.Split(label, ", ") {
		if l == name {
			return true
		}
	}
	return false
}
