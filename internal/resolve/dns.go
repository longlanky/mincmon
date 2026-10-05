// Package resolve looks up A/AAAA records via user-specified DNS servers
// or the system resolver.
package resolve

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"time"

	"github.com/miekg/dns"
)

const timeout = 3 * time.Second

// Domain resolves domain to a de-duplicated, sorted set of addresses.
// With resolvers, each server is queried directly (no /etc/hosts or search
// domains); otherwise the system resolver is used.
func Domain(domain string, resolvers []netip.Addr, wantA, wantAAAA bool) []netip.Addr {
	set := map[netip.Addr]bool{}
	if len(resolvers) > 0 {
		var qtypes []uint16
		if wantA {
			qtypes = append(qtypes, dns.TypeA)
		}
		if wantAAAA {
			qtypes = append(qtypes, dns.TypeAAAA)
		}
		for _, qt := range qtypes {
			for _, r := range resolvers {
				for _, a := range query(domain, r, qt) {
					set[a] = true
				}
			}
		}
	} else {
		var nets []string
		if wantA {
			nets = append(nets, "ip4")
		}
		if wantAAAA {
			nets = append(nets, "ip6")
		}
		for _, n := range nets {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			addrs, _ := net.DefaultResolver.LookupNetIP(ctx, n, domain)
			cancel()
			for _, a := range addrs {
				set[a.Unmap()] = true
			}
		}
	}
	out := make([]netip.Addr, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

func query(domain string, server netip.Addr, qtype uint16) []netip.Addr {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(domain), qtype)
	addr := netip.AddrPortFrom(server, 53).String()

	c := &dns.Client{Timeout: timeout}
	resp, _, err := c.Exchange(m, addr)
	if err == nil && resp.Truncated {
		c.Net = "tcp"
		resp, _, err = c.Exchange(m, addr)
	}
	if err != nil || resp.Rcode != dns.RcodeSuccess {
		return nil
	}
	var out []netip.Addr
	for _, rr := range resp.Answer {
		var ip net.IP
		switch v := rr.(type) {
		case *dns.A:
			ip = v.A
		case *dns.AAAA:
			ip = v.AAAA
		default:
			continue // e.g. CNAMEs in the chain
		}
		if a, ok := netip.AddrFromSlice(ip); ok {
			if qtype == dns.TypeA {
				a = a.Unmap()
			}
			out = append(out, a)
		}
	}
	return out
}
