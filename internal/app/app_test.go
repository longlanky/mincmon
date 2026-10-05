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
	}}
	entries := targets.ParseEntries("10.0.0.0/24, 10.1.0.0/22, 10.9.9.9")
	got, err := ResolveEntries(entries, s, DNSOptions{})
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
	_, err := ResolveEntries(targets.ParseEntries("10.0.0.0/24"), &script{}, DNSOptions{})
	if err != ErrCancelled {
		t.Errorf("err = %v", err)
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
