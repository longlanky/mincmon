// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

package resolve

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// Needs network access; skipped when the lookup returns nothing.
func TestDomain(t *testing.T) {
	cf := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	got := Domain("one.one.one.one", cf, true, true)
	if len(got) == 0 {
		t.Skip("no DNS access")
	}
	has := map[string]bool{}
	for _, a := range got {
		has[a.String()] = true
	}
	if !has["1.1.1.1"] || !has["2606:4700:4700::1111"] {
		t.Errorf("custom resolver: got %v", got)
	}
	if v4 := Domain("one.one.one.one", cf, true, false); len(v4) == 0 || !v4[0].Is4() {
		t.Errorf("A only: got %v", v4)
	}
	if sys := Domain("one.one.one.one", nil, true, false); len(sys) == 0 {
		t.Errorf("system resolver returned nothing")
	}
}

func TestPTRLocalServerAndCNAME(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	srv := &dns.Server{
		PacketConn: pc,
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			m := new(dns.Msg)
			m.SetReply(r)
			q := r.Question[0]
			switch strings.ToLower(q.Name) {
			case "5.1.0.192.in-addr.arpa.":
				// Simulate RFC 2317 CNAME delegation.
				m.Answer = append(m.Answer, &dns.CNAME{
					Hdr:    dns.RR_Header{Name: q.Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60},
					Target: "5.0-25.1.0.192.in-addr.arpa.",
				})
			case "5.0-25.1.0.192.in-addr.arpa.":
				m.Answer = append(m.Answer, &dns.PTR{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60},
					Ptr: "host.example.com.",
				})
			}
			_ = w.WriteMsg(m)
		}),
	}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()
	<-ready
	defer srv.Shutdown()

	arpa, err := dns.ReverseAddr("192.0.1.5")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	exchangeAddr(arpa, pc.LocalAddr().String(), dns.TypePTR, func(rr dns.RR) bool {
		if v, ok := rr.(*dns.PTR); ok {
			got = append(got, strings.TrimRight(v.Ptr, "."))
			return true
		}
		return false
	})
	if len(got) != 1 || got[0] != "host.example.com" {
		t.Fatalf("PTR via CNAME = %v", got)
	}
	if invalid := PTR(netip.Addr{}, nil); len(invalid) != 0 {
		t.Fatalf("invalid addr PTR = %v", invalid)
	}
}

func TestPTRNetwork(t *testing.T) {
	cf := []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	got := PTR(netip.MustParseAddr("1.1.1.1"), cf)
	if len(got) == 0 {
		t.Skip("no DNS access")
	}
	if got[0] != "one.one.one.one" {
		t.Errorf("PTR(1.1.1.1) = %v, want one.one.one.one", got)
	}
}
