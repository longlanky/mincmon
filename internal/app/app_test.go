// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

package app

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"mincmon/internal/monitor"
	"mincmon/internal/targets"
)

// script answers prompts in order and records everything shown.
type script struct {
	answers []string
	shown   []string
}

func (s *script) Prompt(text string) (string, error) {
	s.shown = append(s.shown, text)
	if len(s.answers) == 0 {
		return "", ErrCancelled
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, nil
}

func (s *script) Notify(text string) { s.shown = append(s.shown, text) }

func TestResolveEntriesRefinement(t *testing.T) {
	s := &script{answers: []string{
		"0-5000", // too many -> error, re-prompt
		"1,5-6",  // ok
		"",       // /22 blank -> confirm
		"n",      // declined -> re-prompt
		"",       // blank again
		"y",      // accept first 1024
		"",       // include network+broadcast? no
	}}
	entries := targets.ParseEntries("10.0.0.0/24, 10.1.0.0/22, 10.9.9.9")
	got, err := ResolveEntries(entries, s, DNSOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3+1022+1 {
		t.Errorf("got %d targets", len(got))
	}
	if got[0].Addr.String() != "10.0.0.1" || got[2].Addr.String() != "10.0.0.6" || got[len(got)-1].Addr.String() != "10.9.9.9" {
		t.Errorf("unexpected targets %v ... %v", got[:3], got[len(got)-1])
	}
	if !strings.Contains(strings.Join(s.shown, "\n"), "more than 1024") {
		t.Errorf("expected cap error to be shown: %q", s.shown)
	}
}

func TestResolveEntriesCancel(t *testing.T) {
	_, err := ResolveEntries(targets.ParseEntries("10.0.0.0/24"), &script{}, DNSOptions{}, nil)
	if err != ErrCancelled {
		t.Errorf("err = %v", err)
	}
}

func TestResolveIncludeBaseSession(t *testing.T) {
	sess := &Session{}
	s := &script{answers: []string{"", "a"}} // refine blank, include: always
	got, err := ResolveEntries(targets.ParseEntries("10.0.0.0/24"), s, DNSOptions{}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 256 || got[0].Addr.String() != "10.0.0.0" || got[255].Addr.String() != "10.0.0.255" ||
		got[0].Origin != "10.0.0.0/24" || !sess.IncludeBaseAlways {
		t.Fatalf("includeBase got %d hosts first=%v", len(got), got[:1])
	}
	// "always" sticks: next subnet asks only for refinement.
	s2 := &script{answers: []string{""}}
	got, err = ResolveEntries(targets.ParseEntries("10.1.0.0/24"), s2, DNSOptions{}, sess)
	if err != nil || len(got) != 256 || got[0].Addr.String() != "10.1.0.0" {
		t.Fatalf("session includeBase: %d %v", len(got), err)
	}
	// Without include, network+broadcast are skipped as before.
	s3 := &script{answers: []string{"", ""}}
	got, err = ResolveEntries(targets.ParseEntries("10.2.0.0/24"), s3, DNSOptions{}, &Session{})
	if err != nil || len(got) != 254 || got[0].Addr.String() != "10.2.0.1" {
		t.Fatalf("default expansion: %d %v", len(got), err)
	}
}

func TestResolveRangeAndBadRange(t *testing.T) {
	s := &script{}
	got, err := ResolveEntries(targets.ParseEntries("192.168.1.10-12, 192.168.1.50-10"), s, DNSOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Origin != "192.168.1.10-192.168.1.12" {
		t.Fatalf("range resolve = %+v", got)
	}
	if !strings.Contains(strings.Join(s.shown, "\n"), "Invalid range") {
		t.Errorf("bad range not reported: %q", s.shown)
	}
}

func TestAddCapAndConfirmReport(t *testing.T) {
	m := monitor.New(noop{}, time.Hour, time.Second)
	defer m.Stop()
	var ts []targets.Target
	for i := 0; i < targets.HostCap-1; i++ {
		ts = append(ts, targets.Target{Addr: netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1})})
	}
	m.Add(ts)
	s := &script{answers: []string{"10.9.0.1,10.9.0.2"}}
	if msg := Add(m, s, DNSOptions{}, nil); !strings.Contains(msg, "would exceed") || m.Count() != targets.HostCap-1 {
		t.Errorf("cap msg = %q count=%d", msg, m.Count())
	}
	s2 := &script{answers: []string{"bogus", "2"}}
	if msg := ChangeConfirm(m, s2); !strings.Contains(msg, "2 consecutive") || m.Confirm() != 2 {
		t.Errorf("confirm msg = %q n=%d", msg, m.Confirm())
	}
	rep := FormatReport(m, time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC))
	if !strings.Contains(rep, "confirm 2") || !strings.Contains(rep, "(no state changes") {
		t.Errorf("report:\n%s", rep)
	}
}

type noop struct{}

func (noop) Probe(ctx context.Context, _ netip.Addr, _ time.Duration) (time.Duration, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (noop) Name() string { return "noop" }
func (noop) Close() error { return nil }

func TestRemoveMatching(t *testing.T) {
	m := monitor.New(noop{}, time.Hour, time.Second)
	defer m.Stop()
	p := netip.MustParseAddr
	m.Add([]targets.Target{
		{Addr: p("10.0.0.1")}, {Addr: p("10.0.0.2"), Domain: "a.com, b.com"},
		{Addr: p("10.0.1.1")}, {Addr: p("2001:db8::1"), Domain: "b.com"},
	})
	if msg := RemoveMatching(m, "10.0.0.0/24"); msg != "Removed 2 host(s)." {
		t.Errorf("subnet remove: %s", msg)
	}
	if msg := RemoveMatching(m, "b.com"); msg != "Removed 1 host(s)." {
		t.Errorf("domain remove: %s", msg)
	}
	if msg := RemoveMatching(m, "c.com, 10.9.9.9"); msg != "Nothing matched." {
		t.Errorf("no-match remove: %s", msg)
	}
	if ts := m.Targets(); len(ts) != 1 || ts[0].Addr != p("10.0.1.1") {
		t.Errorf("left: %v", ts)
	}
}

func TestResolvePTR(t *testing.T) {
	orig := ptrLookup
	defer func() { ptrLookup = orig }()

	m := monitor.New(noop{}, time.Hour, time.Second)
	defer m.Stop()
	p := netip.MustParseAddr
	ipOnly := p("192.0.2.10")
	withHost := p("192.0.2.20")
	res := p("9.9.9.9")
	m.Add([]targets.Target{
		{Addr: ipOnly},
		{Addr: withHost, Domain: "orig.example.com", Resolver: res},
	})

	// No selection / unmonitored.
	if msg := ResolvePTR(m, &script{}, DNSOptions{}, netip.Addr{}); msg != "No host selected." {
		t.Errorf("empty sel = %q", msg)
	}
	if msg := ResolvePTR(m, &script{}, DNSOptions{}, p("192.0.2.99")); !strings.Contains(msg, "no longer monitored") {
		t.Errorf("unmonitored = %q", msg)
	}

	// Cancel at resolver prompt.
	if msg := ResolvePTR(m, &script{}, DNSOptions{}, ipOnly); msg != "PTR lookup cancelled." {
		t.Errorf("cancel = %q", msg)
	}

	// No PTR record found.
	ptrLookup = func(netip.Addr, []netip.Addr) []string { return nil }
	if msg := ResolvePTR(m, &script{answers: []string{""}}, DNSOptions{}, ipOnly); !strings.Contains(msg, "No PTR record") {
		t.Errorf("no ptr = %q", msg)
	}

	// Set hostname on IP-only host without confirmation, passing custom resolver.
	var gotResolvers []netip.Addr
	ptrLookup = func(_ netip.Addr, resolvers []netip.Addr) []string {
		gotResolvers = resolvers
		return []string{"ptr1.example.com"}
	}
	msg := ResolvePTR(m, &script{answers: []string{"1.1.1.1"}}, DNSOptions{}, ipOnly)
	if !strings.Contains(msg, "Set hostname") || len(gotResolvers) != 1 || gotResolvers[0] != p("1.1.1.1") {
		t.Fatalf("set blank domain: msg=%q resolvers=%v", msg, gotResolvers)
	}
	if tgt, _ := m.Target(ipOnly); tgt.Domain != "ptr1.example.com" {
		t.Fatalf("ipOnly domain = %q", tgt.Domain)
	}

	// Same hostname (case-insensitive) does not prompt to replace.
	ptrLookup = func(netip.Addr, []netip.Addr) []string { return []string{"ORIG.EXAMPLE.COM"} }
	sSame := &script{answers: []string{""}}
	if msg := ResolvePTR(m, sSame, DNSOptions{}, withHost); !strings.Contains(msg, "already") || len(sSame.shown) != 1 {
		t.Errorf("same hostname: msg=%q prompts=%d", msg, len(sSame.shown))
	}

	// Different hostname declined -> unchanged.
	ptrLookup = func(netip.Addr, []netip.Addr) []string { return []string{"new.example.com"} }
	sNo := &script{answers: []string{"", "n"}}
	if msg := ResolvePTR(m, sNo, DNSOptions{}, withHost); !strings.Contains(msg, "left unchanged") {
		t.Errorf("declined: msg=%q", msg)
	}
	if tgt, _ := m.Target(withHost); tgt.Domain != "orig.example.com" {
		t.Errorf("declined modified domain: %q", tgt.Domain)
	}

	// Different hostname confirmed with "y" -> replaced, resolver provenance kept.
	sYes := &script{answers: []string{"", "y"}}
	if msg := ResolvePTR(m, sYes, DNSOptions{}, withHost); !strings.Contains(msg, "Updated hostname") {
		t.Errorf("confirmed: msg=%q", msg)
	}
	if tgt, _ := m.Target(withHost); tgt.Domain != "new.example.com" || tgt.Resolver != res {
		t.Errorf("after confirm: %+v", tgt)
	}

	// ResolversSet skips resolver prompt.
	sFlag := &script{answers: []string{"y"}}
	ptrLookup = func(_ netip.Addr, resolvers []netip.Addr) []string {
		gotResolvers = resolvers
		return []string{"flag.example.com"}
	}
	opt := DNSOptions{ResolversSet: true, Resolvers: []netip.Addr{p("8.8.8.8")}}
	if msg := ResolvePTR(m, sFlag, opt, withHost); !strings.Contains(msg, "Updated hostname") || len(sFlag.shown) != 1 || gotResolvers[0] != p("8.8.8.8") {
		t.Errorf("flag resolver: msg=%q shown=%v resolvers=%v", msg, sFlag.shown, gotResolvers)
	}
}
