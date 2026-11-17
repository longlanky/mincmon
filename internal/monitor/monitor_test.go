// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"mincmon/internal/probe"
	"mincmon/internal/targets"
)

// fake answers for 10.0.0.0/8 and times out for everything else.
type fake struct{}

func (fake) Probe(ctx context.Context, a netip.Addr, _ time.Duration) (time.Duration, error) {
	if a.As4()[0] == 10 {
		return 5 * time.Millisecond, nil
	}
	return 0, probe.ErrTimeout
}
func (fake) Name() string { return "fake" }
func (fake) Close() error { return nil }

// counting records how many probes were sent and blocks until each is
// released, so the test controls probe timing precisely.
type counting struct {
	mu      sync.Mutex
	count   int
	release chan struct{}
}

func (c *counting) Probe(ctx context.Context, _ netip.Addr, _ time.Duration) (time.Duration, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	return time.Millisecond, nil
}
func (*counting) Name() string { return "counting" }
func (*counting) Close() error { return nil }

func TestPauseResume(t *testing.T) {
	c := &counting{release: make(chan struct{})}
	m := New(c, 5*time.Millisecond, time.Second)
	defer m.Stop()
	addr := netip.MustParseAddr("10.0.0.1")
	m.Add([]targets.Target{{Addr: addr}})

	// Let the first probe start, then pause before releasing it.
	<-time.After(50 * time.Millisecond)
	m.Pause()
	if !m.Paused() {
		t.Fatal("Paused() false after Pause()")
	}
	close(c.release) // finish probe #1; the loop must now block on the gate

	<-time.After(50 * time.Millisecond)
	n := func() int { c.mu.Lock(); defer c.mu.Unlock(); return c.count }()
	if n != 1 {
		t.Fatalf("probes sent while paused: %d", n)
	}
	m.Resume()
	if m.Paused() {
		t.Fatal("Paused() true after Resume()")
	}
	<-time.After(50 * time.Millisecond)
	if n = func() int { c.mu.Lock(); defer c.mu.Unlock(); return c.count }(); n < 2 {
		t.Fatalf("no probes after Resume() (count %d)", n)
	}
	m.Stop()
}

// stuck blocks every probe until its context ends, so tests can drive
// record() by hand without worker interference.
type stuck struct{}

func (stuck) Probe(ctx context.Context, _ netip.Addr, _ time.Duration) (time.Duration, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (stuck) Name() string { return "stuck" }
func (stuck) Close() error { return nil }

func fakeClock(m *Monitor) *time.Time {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	return &now
}

func TestConfirmAndBaseline(t *testing.T) {
	m := New(stuck{}, time.Hour, time.Second)
	defer m.Stop()
	now := fakeClock(m)
	a := netip.MustParseAddr("10.0.0.1")
	m.Add([]targets.Target{{Addr: a}})
	up := func() { m.record(a, time.Millisecond, nil) }
	down := func() { m.record(a, 0, probe.ErrTimeout) }

	up() // baseline: silent, immediate
	if ev, _, _ := m.Events(); len(ev) != 0 {
		t.Fatalf("baseline produced events %v", ev)
	}
	down()
	down()
	if s := m.Snapshot()[0]; s.State != Up || s.CandState != Down || s.CandCount != 2 {
		t.Fatalf("mid-confirm: %+v", s)
	}
	up() // resets the streak
	*now = now.Add(4 * time.Minute)
	down()
	down()
	down() // third consecutive: confirmed flip
	s := m.Snapshot()[0]
	if s.State != Down || s.CandCount != 0 || s.Changes != 1 {
		t.Fatalf("after confirm: %+v", s)
	}
	ev, dropped, seq := m.Events()
	if len(ev) != 1 || dropped != 0 || seq != 1 || ev[0].Kind != EventChange ||
		ev[0].From != Up || ev[0].To != Down || ev[0].PrevDuration != 4*time.Minute {
		t.Fatalf("events = %+v dropped=%d seq=%d", ev, dropped, seq)
	}
	if got := m.SetConfirm(99); got != 10 || m.Confirm() != 10 {
		t.Fatalf("SetConfirm clamp = %d", got)
	}
}

func TestFlapSlowAndSettle(t *testing.T) {
	m := New(stuck{}, time.Hour, time.Second)
	defer m.Stop()
	m.SetConfirm(1)
	now := fakeClock(m)
	a := netip.MustParseAddr("10.0.0.2")
	m.Add([]targets.Target{{Addr: a}})
	flip := func() { // record the opposite of the confirmed state
		if m.Snapshot()[0].State == Up {
			m.record(a, 0, probe.ErrTimeout)
		} else {
			m.record(a, time.Millisecond, nil)
		}
	}
	m.record(a, time.Millisecond, nil) // baseline UP
	for i := 0; i < 4; i++ {
		*now = now.Add(90 * time.Second)
		flip()
	}
	ev, _, _ := m.Events()
	// 3 plain changes, then flap entry instead of the 4th change line.
	if len(ev) != 4 || ev[0].Kind != EventFlapStart || ev[0].Tier != FlapSlow {
		t.Fatalf("flap events = %+v", ev)
	}
	if s := m.Snapshot()[0]; s.Flap != FlapSlow {
		t.Fatalf("flap tier = %v", s.Flap)
	}
	*now = now.Add(90 * time.Second)
	flip() // suppressed while flapping (state still flips internally)
	if ev, _, _ := m.Events(); len(ev) != 4 {
		t.Fatalf("flapping produced extra events %v", ev)
	}
	// Settle: probe agrees with the confirmed state after 5m stable.
	cur := m.Snapshot()[0].State
	*now = now.Add(FlapSettleSlow + time.Second)
	if cur == Up {
		m.record(a, time.Millisecond, nil)
	} else {
		m.record(a, 0, probe.ErrTimeout)
	}
	ev, _, _ = m.Events()
	if len(ev) != 5 || ev[0].Kind != EventFlapSettled || ev[0].To != cur || ev[0].Changes != 2 {
		t.Fatalf("settled events = %+v", ev)
	}
	if s := m.Snapshot()[0]; s.Flap != FlapNone {
		t.Fatalf("flap after settle = %v", s.Flap)
	}
}

func TestFlapRapidAndSettle(t *testing.T) {
	m := New(stuck{}, time.Hour, time.Second)
	defer m.Stop()
	m.SetConfirm(1)
	now := fakeClock(m)
	a := netip.MustParseAddr("10.0.0.3")
	m.Add([]targets.Target{{Addr: a}})
	m.record(a, time.Millisecond, nil) // baseline UP
	state := Up
	for i := 0; i < 4; i++ {
		*now = now.Add(10 * time.Second)
		if state == Up {
			m.record(a, 0, probe.ErrTimeout)
			state = Down
		} else {
			m.record(a, time.Millisecond, nil)
			state = Up
		}
	}
	ev, _, _ := m.Events()
	if len(ev) != 4 || ev[0].Kind != EventFlapRapid || ev[0].Tier != FlapRapid {
		t.Fatalf("rapid events = %+v", ev)
	}
	*now = now.Add(FlapSettleRapid + time.Second)
	m.record(a, time.Millisecond, nil) // agrees with settled UP
	ev, _, _ = m.Events()
	if len(ev) != 5 || ev[0].Kind != EventFlapSettled || ev[0].Tier != FlapRapid || ev[0].To != Up {
		t.Fatalf("rapid settled = %+v", ev)
	}
}

func TestEventCap(t *testing.T) {
	m := New(stuck{}, time.Hour, time.Second)
	defer m.Stop()
	m.SetConfirm(1)
	var ts []targets.Target
	for i := 0; i < EventCap+100; i++ {
		ts = append(ts, targets.Target{Addr: netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 1})})
	}
	m.Add(ts)
	for _, tg := range ts {
		m.record(tg.Addr, time.Millisecond, nil) // baseline
		m.record(tg.Addr, 0, probe.ErrTimeout)   // one change each
	}
	ev, dropped, seq := m.Events()
	if len(ev) != EventCap || dropped != 100 || seq != uint64(EventCap+100) || ev[0].Seq != seq || ev[len(ev)-1].Seq != 101 {
		t.Fatalf("cap: len=%d dropped=%d seq=%d", len(ev), dropped, seq)
	}
}

func TestMonitor(t *testing.T) {
	m := New(fake{}, 10*time.Millisecond, time.Second)
	defer m.Stop()
	up, down := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("192.0.2.1")
	added := m.Add([]targets.Target{{Addr: up}, {Addr: down, Domain: "x.test"}})
	if len(added) != 2 {
		t.Fatalf("added %v", added)
	}
	// Re-adding fills in a missing label but doesn't duplicate
	if a := m.Add([]targets.Target{{Addr: up, Domain: "up.test"}}); len(a) != 0 {
		t.Fatalf("re-add added %v", a)
	}
	time.Sleep(150 * time.Millisecond)
	snap := m.Snapshot()
	if len(snap) != 2 || snap[0].Addr != up || snap[0].Domain != "up.test" {
		t.Fatalf("snapshot %+v", snap)
	}
	if snap[0].State != Up || snap[0].OK == 0 || snap[0].Latency != 5*time.Millisecond || snap[0].Loss() != 0 {
		t.Errorf("up host: %+v", snap[0])
	}
	if snap[1].State != Down || snap[1].Fail == 0 || snap[1].Loss() != 1 {
		t.Errorf("down host: %+v", snap[1])
	}
	if r := m.Remove([]netip.Addr{up, up}); len(r) != 1 {
		t.Errorf("removed %v", r)
	}
	if ts := m.Targets(); len(ts) != 1 || ts[0].Addr != down {
		t.Errorf("targets after remove: %v", ts)
	}
}

func TestTargetAndSetDomain(t *testing.T) {
	m := New(fake{}, time.Hour, time.Second)
	defer m.Stop()
	addr := netip.MustParseAddr("192.0.2.10")
	res := netip.MustParseAddr("1.1.1.1")
	m.Add([]targets.Target{{Addr: addr, Domain: "old.example.com", Resolver: res}})

	got, ok := m.Target(addr)
	if !ok || got.Domain != "old.example.com" || got.Resolver != res {
		t.Fatalf("Target = %+v, %v", got, ok)
	}
	if !m.SetDomain(addr, "new.example.com") {
		t.Fatal("SetDomain returned false for monitored addr")
	}
	got, ok = m.Target(addr)
	if !ok || got.Domain != "new.example.com" || got.Resolver != res {
		t.Fatalf("Target after SetDomain = %+v, %v", got, ok)
	}
	if m.SetDomain(netip.MustParseAddr("192.0.2.99"), "x.example.com") {
		t.Fatal("SetDomain returned true for unmonitored addr")
	}
}
