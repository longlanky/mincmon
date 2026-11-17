// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

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

	// Range entries ("192.168.1.10-192.168.1.50", "192.168.1.10-50",
	// "2001:db8::10-2001:db8::50") are Network entries spanning
	// RangeStart..RangeEnd inclusive; they need not align to a subnet.
	Range      bool
	RangeStart netip.Addr
	RangeEnd   netip.Addr
	// BadRange is set (with the token kept in Name) when the input clearly
	// tried to be a range — its left side parses as an IP — but is invalid,
	// so callers can complain instead of DNS-looking-up nonsense.
	BadRange string
}

// RangeLabel is the canonical "start-end" spelling of a range entry.
func (e Entry) RangeLabel() string {
	if !e.Range {
		return ""
	}
	return e.RangeStart.String() + "-" + e.RangeEnd.String()
}

// Target is a monitored host, its (possibly merged) domain label, and the
// DNS resolver that produced the entry (zero = system resolver). The
// resolver is provenance: re-resolving the domain queries it again.
type Target struct {
	Addr     netip.Addr
	Domain   string
	Resolver netip.Addr // zero value = system resolver
	// Origin is the subnet ("10.0.0.0/24") or range
	// ("192.168.1.10-192.168.1.50") the host was expanded from, used to
	// group hosts in the deltas view. Empty for single IPs, domain results
	// and list-loaded hosts. The first origin seen wins; it is not saved
	// to .ml.txt files, so reloaded hosts have no origin.
	Origin string
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
	if e, ok := parseRange(part); ok {
		return e
	}
	return Entry{Kind: Domain, Name: part}
}

// parseRange parses an explicit inclusive address range. The full form is
// "start-end" for IPv4 or IPv6; IPv4 also accepts a shorthand whose right
// side is just the last octet ("192.168.1.10-50"). It reports ok=false when
// the token does not look like a range at all (its left side is not an IP),
// so ordinary hyphenated domains are unaffected. A token whose left side
// parses as an IP but whose range is malformed yields a BadRange entry.
func parseRange(part string) (Entry, bool) {
	loS, hiS, found := strings.Cut(part, "-")
	if !found || strings.Contains(hiS, "-") {
		return Entry{}, false
	}
	lo, err := netip.ParseAddr(strings.TrimSpace(loS))
	if err != nil {
		return Entry{}, false
	}
	bad := func(reason string) (Entry, bool) {
		return Entry{Kind: Network, Name: part, BadRange: reason}, true
	}
	if lo.Zone() != "" {
		return bad("zones are not supported in ranges")
	}
	hiS = strings.TrimSpace(hiS)
	if hi, err := netip.ParseAddr(hiS); err == nil {
		if hi.Zone() != "" {
			return bad("zones are not supported in ranges")
		}
		if lo.Is4() != hi.Is4() || lo.Is6() != hi.Is6() {
			return bad("range ends are different address families")
		}
		if hi.Compare(lo) < 0 {
			return bad("range end is before its start")
		}
		return Entry{Kind: Network, Range: true, RangeStart: lo, RangeEnd: hi}, true
	}
	if lo.Is4() {
		// Shorthand: right side is the final octet, decimal.
		if n, err := strconv.ParseUint(hiS, 10, 8); err == nil {
			b := lo.As4()
			hi := netip.AddrFrom4([4]byte{b[0], b[1], b[2], byte(n)})
			if hi.Compare(lo) < 0 {
				return bad("range end is before its start")
			}
			return Entry{Kind: Network, Range: true, RangeStart: lo, RangeEnd: hi}, true
		}
	}
	return bad(fmt.Sprintf("bad range end %q", hiS))
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
			remaining := cap - len(offsets)
			if remaining < 0 {
				return nil, true, tooMany
			}
			if span.cmp(Offset{0, uint64(remaining)}) >= 0 {
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
// includeBase keeps the first address (IPv4 network / IPv6 subnet-router
// anycast) and, for IPv4, the broadcast address in an unrefined expansion
// instead of skipping them. Range entries expand to every address from
// start to end inclusive, and are skipped entirely (with a warning) when
// they span more than cap hosts. Truncation to cap, out-of-range offsets,
// and empty results are reported through warn.
func Expand(e Entry, offsets []Offset, refined bool, includeBase bool, cap int, warn func(string)) []netip.Addr {
	if e.Single {
		return []netip.Addr{e.Addr}
	}
	if e.Range {
		start, end := addrToU128(e.RangeStart), addrToU128(e.RangeEnd)
		if end.sub(start).cmp(Offset{0, uint64(cap)}) >= 0 { // count = span+1 > cap
			warn(fmt.Sprintf("! Range %s expands to more than %d hosts; skipped.", e.RangeLabel(), cap))
			return nil
		}
		var out []netip.Addr
		for v := start; ; v, _ = v.add(Offset{0, 1}) {
			a, _ := u128ToAddr(v, e.RangeStart)
			out = append(out, a)
			if v == end {
				break
			}
		}
		return out
	}
	p := e.Prefix
	base := addrToU128(p.Addr())
	var out []netip.Addr

	if refined {
		skipped := 0
		for _, off := range offsets {
			if len(out) >= cap {
				warn(fmt.Sprintf("! Refinement for %s truncated to %d hosts.", p, cap))
				break
			}
			v, over := base.add(off)
			if over {
				skipped++
				continue
			}
			a, ok := u128ToAddr(v, p.Addr())
			if ok && p.Contains(a) {
				out = append(out, a)
			} else {
				skipped++
			}
		}
		if skipped > 0 {
			warn(fmt.Sprintf("! %d refined offset(s) outside %s skipped.", skipped, p))
		}
		if len(out) == 0 {
			warn(fmt.Sprintf("! Refinement for %s matched no hosts.", p))
		}
		return out
	}

	hostBits := p.Addr().BitLen() - p.Bits()
	last := base
	switch {
	case hostBits >= 128:
		last.hi = ^uint64(0)
		last.lo = ^uint64(0)
	case hostBits == 64:
		last.lo = ^uint64(0)
	case hostBits > 64:
		last.lo = ^uint64(0)
		last.hi |= (uint64(1) << uint(hostBits-64)) - 1
	case hostBits > 0:
		last.lo |= (uint64(1) << uint(hostBits)) - 1
	}
	start, end := base, last
	if includeBase {
		// Keep network/broadcast (IPv4) and the subnet-router anycast
		// address (IPv6): the user explicitly asked for them.
	} else if p.Addr().Is4() && hostBits > 1 {
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

// SkipsBase reports whether an unrefined expansion of e would skip its
// first address (and, for IPv4, its last): true for IPv4 prefixes with
// more than one host bit and for IPv6 prefixes likewise. Callers use it to
// decide whether asking "include network/broadcast?" is meaningful.
func (e Entry) SkipsBase() bool {
	if e.Single || e.Range || e.Kind != Network {
		return false
	}
	return e.Prefix.Addr().BitLen()-e.Prefix.Bits() > 1
}

// HostCount returns min(number of addresses in p, limit).
func HostCount(p netip.Prefix, limit int) int {
	hostBits := p.Addr().BitLen() - p.Bits()
	if hostBits < 0 {
		return 0
	}
	if hostBits >= 31 {
		return limit
	}
	// hostBits < 31 here, so 1<<hostBits fits even 32-bit ints.
	if n := 1 << uint(hostBits); n > limit {
		return limit
	} else {
		return n
	}
}

// Dedupe de-duplicates by address, keeping first-seen order. When one
// address was resolved from several domains their labels are merged as
// "a.com, b.com". The first recorded resolver wins (zero = system).
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
		if !out[i].Resolver.IsValid() {
			out[i].Resolver = t.Resolver
		}
		if out[i].Origin == "" {
			out[i].Origin = t.Origin
		}
	}
	return out
}

// HasLabel reports whether a (possibly merged) domain label contains name.
// Labels are comma-separated; surrounding spaces are ignored so hand-edited
// lists like "a.com,b.com" still match.
func HasLabel(label, name string) bool {
	name = strings.TrimSpace(name)
	for _, l := range strings.Split(label, ",") {
		if strings.TrimSpace(l) == name {
			return true
		}
	}
	return false
}

// LooksLikeNetwork reports whether s was probably meant as a subnet but
// failed to parse (e.g. "10.0.0.0/33" or "10.0.0.0/255.0.255.0"). Callers
// use it to warn instead of silently treating the typo as a domain.
func LooksLikeNetwork(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "/") {
		return false
	}
	if _, ok := parsePrefix(s); ok {
		return false
	}
	addrS, _, _ := strings.Cut(s, "/")
	// Contains a parseable IP on the left, or dotted netmask on the right:
	// almost certainly a CIDR typo, not a domain.
	if _, err := netip.ParseAddr(strings.TrimSpace(addrS)); err == nil {
		return true
	}
	return false
}
