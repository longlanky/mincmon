// Package monitor runs one goroutine per target, probing it on an
// interval and recording state in a shared, insertion-ordered table.
package monitor

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"mincmon/internal/probe"
	"mincmon/internal/targets"
)

// HistoryLen is how many recent probe results are kept per host.
const HistoryLen = 60

type State int

const (
	Pending State = iota
	Up
	Down
)

func (s State) String() string {
	return [...]string{"PENDING", "UP", "DOWN"}[s]
}

// Status is a point-in-time copy of one host's state.
type Status struct {
	Addr    netip.Addr
	Domain  string
	State   State
	Latency time.Duration // last successful RTT (valid when State == Up)
	OK      int
	Fail    int
	Changed time.Time       // when State last changed
	Err     string          // last non-timeout probe error, if any
	History []time.Duration // oldest first; <0 marks a lost probe
}

// Loss returns the fraction of probes lost within the history window.
func (s Status) Loss() float64 {
	if len(s.History) == 0 {
		return 0
	}
	lost := 0
	for _, h := range s.History {
		if h < 0 {
			lost++
		}
	}
	return float64(lost) / float64(len(s.History))
}

type host struct {
	Status
	cancel context.CancelFunc
}

// Monitor owns the target table and the worker goroutines.
type Monitor struct {
	Interval time.Duration
	Timeout  time.Duration
	prober   probe.Prober

	mu       sync.RWMutex
	order    []netip.Addr
	hosts    map[netip.Addr]*host
	spawnSeq int
	ctx      context.Context
	stop     context.CancelFunc
}

func New(p probe.Prober, interval, timeout time.Duration) *Monitor {
	ctx, stop := context.WithCancel(context.Background())
	return &Monitor{
		Interval: interval, Timeout: timeout, prober: p,
		hosts: map[netip.Addr]*host{}, ctx: ctx, stop: stop,
	}
}

// Add starts monitoring new targets and returns the newly added addresses.
// For hosts already monitored, a domain label is filled in if it had none.
func (m *Monitor) Add(items []targets.Target) []netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	var added []netip.Addr
	for _, t := range items {
		if h, ok := m.hosts[t.Addr]; ok {
			if h.Domain == "" {
				h.Domain = t.Domain
			}
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		h := &host{Status: Status{Addr: t.Addr, Domain: t.Domain, Changed: time.Now()}, cancel: cancel}
		m.hosts[t.Addr] = h
		m.order = append(m.order, t.Addr)
		added = append(added, t.Addr)
		// Stagger start times so large lists don't burst all at once
		delay := time.Duration(m.spawnSeq%40) * m.Interval / 40
		m.spawnSeq++
		go m.worker(ctx, t.Addr, delay)
	}
	return added
}

// Remove stops monitoring the given addresses and returns those removed.
func (m *Monitor) Remove(addrs []netip.Addr) []netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	drop := map[netip.Addr]bool{}
	var removed []netip.Addr
	for _, a := range addrs {
		if h, ok := m.hosts[a]; ok && !drop[a] {
			h.cancel()
			delete(m.hosts, a)
			drop[a] = true
			removed = append(removed, a)
		}
	}
	if len(drop) > 0 {
		kept := m.order[:0]
		for _, a := range m.order {
			if !drop[a] {
				kept = append(kept, a)
			}
		}
		m.order = kept
	}
	return removed
}

// Snapshot returns copies of every host's status in insertion order.
func (m *Monitor) Snapshot() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.order))
	for _, a := range m.order {
		s := m.hosts[a].Status
		s.History = append([]time.Duration(nil), s.History...)
		out = append(out, s)
	}
	return out
}

// Targets returns the monitored addresses and labels, for saving.
func (m *Monitor) Targets() []targets.Target {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]targets.Target, 0, len(m.order))
	for _, a := range m.order {
		out = append(out, targets.Target{Addr: a, Domain: m.hosts[a].Domain})
	}
	return out
}

func (m *Monitor) ProberName() string { return m.prober.Name() }

// Stop cancels every worker.
func (m *Monitor) Stop() { m.stop() }

func (m *Monitor) worker(ctx context.Context, addr netip.Addr, delay time.Duration) {
	if !sleep(ctx, delay) {
		return
	}
	for {
		rtt, err := m.prober.Probe(ctx, addr, m.Timeout)
		if ctx.Err() != nil {
			return
		}
		m.record(addr, rtt, err)
		if !sleep(ctx, m.Interval) {
			return
		}
	}
}

func (m *Monitor) record(addr netip.Addr, rtt time.Duration, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hosts[addr]
	if !ok {
		return
	}
	state := Up
	if err != nil {
		state = Down
		rtt = -1
		h.Fail++
		if errors.Is(err, probe.ErrTimeout) {
			h.Err = ""
		} else {
			h.Err = err.Error()
		}
	} else {
		h.OK++
		h.Latency = rtt
		h.Err = ""
	}
	if state != h.State {
		h.State = state
		h.Changed = time.Now()
	}
	h.History = append(h.History, rtt)
	if len(h.History) > HistoryLen {
		h.History = h.History[len(h.History)-HistoryLen:]
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
