package monitor

import (
	"context"
	"net/netip"
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
