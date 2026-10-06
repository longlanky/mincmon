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
	mu     sync.Mutex
	count  int
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
func (*counting) Name() string   { return "counting" }
func (*counting) Close() error   { return nil }

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
