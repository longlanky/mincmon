// Package resolve looks up A/AAAA records via user-specified DNS servers
// or the system resolver.
package resolve

import (
	"context"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

const timeout = 3 * time.Second

// maxCNAMEChase is how many CNAME hops to follow when a server returns
// only a CNAME without glue records.
const maxCNAMEChase = 8

// Domain resolves domain to a de-duplicated, sorted set of addresses.
// With resolvers, each server is queried directly (no /etc/hosts or search
// domains); otherwise the system resolver is used.
func Domain(domain string, resolvers []netip.Addr, wantA, wantAAAA bool) []netip.Addr {
	set := map[netip.Addr]bool{}
	var mu sync.Mutex
	add := func(a netip.Addr) {
		mu.Lock()
		set[a] = true
		mu.Unlock()
	}
	if len(resolvers) > 0 {
		var qtypes []uint16
		if wantA {
			qtypes = append(qtypes, dns.TypeA)
		}
		if wantAAAA {
			qtypes = append(qtypes, dns.TypeAAAA)
		}
		var wg sync.WaitGroup
		for _, qt := range qtypes {
			for _, r := range resolvers {
				wg.Add(1)
				go func(qt uint16, r netip.Addr) {
					defer wg.Done()
					for _, a := range query(domain, r, qt) {
						add(a)
					}
				}(qt, r)
			}
		}
		wg.Wait()
	} else {
		var nets []string
		if wantA {
			nets = append(nets, "ip4")
		}
		if wantAAAA {
			nets = append(nets, "ip6")
		}
		var wg sync.WaitGroup
		for _, n := range nets {
			wg.Add(1)
			go func(n string) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				addrs, _ := net.DefaultResolver.LookupNetIP(ctx, n, domain)
				for _, a := range addrs {
					add(a.Unmap())
				}
			}(n)
		}
		wg.Wait()
	}
	out := make([]netip.Addr, 0, len(set))
	for a := range set {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

func query(domain string, server netip.Addr, qtype uint16) []netip.Addr {
	addr := netip.AddrPortFrom(server.WithZone(""), 53).String()
	c := &dns.Client{Timeout: timeout}
	name := dns.Fqdn(domain)
	var out []netip.Addr
	for chase := 0; chase <= maxCNAMEChase; chase++ {
		m := new(dns.Msg)
		m.SetQuestion(name, qtype)
		resp, _, err := c.Exchange(m, addr)
		if err == nil && resp.Truncated {
			c.Net = "tcp"
			resp, _, err = c.Exchange(m, addr)
		}
		if err != nil || resp == nil || resp.Rcode != dns.RcodeSuccess {
			return out
		}
		var cname string
		for _, rr := range resp.Answer {
			switch v := rr.(type) {
			case *dns.A:
				if qtype == dns.TypeA {
					if a, ok := netip.AddrFromSlice(v.A); ok {
						out = append(out, a.Unmap())
					}
				}
			case *dns.AAAA:
				if qtype == dns.TypeAAAA {
					if a, ok := netip.AddrFromSlice(v.AAAA); ok {
						out = append(out, a)
					}
				}
			case *dns.CNAME:
				if cname == "" {
					cname = v.Target
				}
			}
		}
		if len(out) > 0 || cname == "" || strings.EqualFold(cname, name) {
			return out
		}
		// Server gave only a CNAME: follow it on the same server.
		name = cname
		c.Net = ""
	}
	return out
}
