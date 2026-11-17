// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

// Package monitor runs one goroutine per target, probing it on an
// interval and recording state in a shared, insertion-ordered table.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"mincmon/internal/probe"
	"mincmon/internal/targets"
)

// HistoryLen is how many recent probe results are kept per host.
const HistoryLen = 60

// EventCap is how many delta events are retained for the feed/report.
const EventCap = 500

// DefaultConfirm is the number of consecutive opposing probes required
// to confirm a state change when none is configured.
const DefaultConfirm = 3

// Flap thresholds: a host is FLAPPING after FlapSlowCount confirmed
// changes within FlapSlowWindow, and RAPID after FlapRapidCount within
// FlapRapidWindow. A flapping host settles after staying in one confirmed
// state for its tier's settle window.
const (
	FlapSlowCount   = 4
	FlapSlowWindow  = 5 * time.Minute
	FlapSettleSlow  = 5 * time.Minute
	FlapRapidCount  = 4
	FlapRapidWindow = time.Minute
	FlapSettleRapid = 2 * time.Minute
)

type State int

const (
	Pending State = iota
	Up
	Down
)

func (s State) String() string {
	return [...]string{"PENDING", "UP", "DOWN"}[s]
}

// FlapTier marks a host that is changing state too often.
type FlapTier int

const (
	FlapNone FlapTier = iota
	FlapSlow
	FlapRapid
)

func (f FlapTier) String() string {
	return [...]string{"", "FLAPPING", "RAPID"}[f]
}

// EventKind distinguishes delta-feed entries.
type EventKind int

const (
	EventChange EventKind = iota
	EventFlapStart
	EventFlapRapid
	EventFlapSettled
)

// Line renders the event as one plain-text report line, e.g.
// "2026-11-11 09:41:03  192.168.1.5  DOWN→UP  (was down 4m32s)".
func (e Event) Line() string {
	host := e.Addr.String()
	if e.Domain != "" {
		host += " (" + e.Domain + ")"
	}
	ts := e.Time.Format("2006-01-02 15:04:05")
	was := fmt.Sprintf("(was %s %s)", strings.ToLower(e.From.String()), e.PrevDuration.Truncate(time.Second))
	switch e.Kind {
	case EventFlapStart:
		return fmt.Sprintf("%s  %s  started FLAPPING  (%s→%s, %s)", ts, host, e.From, e.To, was)
	case EventFlapRapid:
		return fmt.Sprintf("%s  %s  started RAPID flapping  (%s→%s, %s)", ts, host, e.From, e.To, was)
	case EventFlapSettled:
		return fmt.Sprintf("%s  %s  settled %s  (was %s, %d changes while flapping)", ts, host, e.To, strings.ToLower(e.Tier.String()), e.Changes)
	default: // EventChange
		return fmt.Sprintf("%s  %s  %s→%s  %s", ts, host, e.From, e.To, was)
	}
}

// Event is one recorded state-change (delta) for the feed and report.
// Events are retained even if the host is later removed.
type Event struct {
	Seq    uint64
	Time   time.Time
	Addr   netip.Addr
	Domain string
	Origin string
	Kind   EventKind
	From   State // state before the change (settled events: From == To)
	To     State // confirmed state after the change (settled events: the settled state)
	// PrevDuration is how long the host spent in From before a change.
	PrevDuration time.Duration
	// Tier is the flap tier for flap events; Changes counts confirmed
	// changes while flapping (settled events).
	Tier    FlapTier
	Changes int
}

// Status is a point-in-time copy of one host's state.
type Status struct {
	Addr     netip.Addr
	Domain   string
	Resolver netip.Addr // DNS resolver that produced this entry (zero = system)
	Origin   string     // subnet/range the host was expanded from ("" = none)
	State    State
	Latency  time.Duration // last successful RTT (valid when State == Up)
	OK       int
	Fail     int
	Changed  time.Time       // when the confirmed State last changed
	Err      string          // last non-timeout probe error, if any
	History  []time.Duration // oldest first; <0 marks a lost probe

	// CandState/CandCount report an in-progress, not yet confirmed
	// change: CandCount of the required consecutive probes for
	// CandState have been seen. CandState is Pending when idle.
	CandState State
	CandCount int
	// Flap is the host's flap tier (FlapNone when stable).
	Flap FlapTier
	// Changes is the total confirmed state changes this session.
	Changes int
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

	// changeTimes holds recent confirmed-change instants inside the
	// slow flap window; flapChanges counts changes while flapping.
	changeTimes []time.Time
	flapChanges int
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
	wg       sync.WaitGroup

	confirm  int
	events   []Event // chronological, oldest first, capped at EventCap
	dropped  int     // events dropped because the log was full
	eventSeq uint64
	now      func() time.Time // test seam for flap timing

	// Pause gate: while paused, resume is a fresh open channel every worker
	// blocks on; Resume closes it to release them all at once.
	pauseMu sync.Mutex
	paused  bool
	resume  chan struct{}
}

func New(p probe.Prober, interval, timeout time.Duration) *Monitor {
	ctx, stop := context.WithCancel(context.Background())
	resume := make(chan struct{})
	close(resume) // start unpaused
	return &Monitor{
		Interval: interval, Timeout: timeout, prober: p,
		hosts: map[netip.Addr]*host{}, ctx: ctx, stop: stop,
		resume: resume, confirm: DefaultConfirm, now: time.Now,
	}
}

// Add starts monitoring new targets and returns the newly added addresses.
// For hosts already monitored, domain labels are merged like targets.Dedupe.
func (m *Monitor) Add(items []targets.Target) []netip.Addr {
	m.mu.Lock()
	type spawn struct {
		ctx   context.Context
		addr  netip.Addr
		delay time.Duration
	}
	var toSpawn []spawn
	var added []netip.Addr
	for _, t := range items {
		if h, ok := m.hosts[t.Addr]; ok {
			mergeLabel(&h.Domain, t.Domain)
			if !h.Resolver.IsValid() {
				h.Resolver = t.Resolver
			}
			if h.Origin == "" {
				h.Origin = t.Origin
			}
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		h := &host{Status: Status{Addr: t.Addr, Domain: t.Domain, Resolver: t.Resolver, Origin: t.Origin, Changed: time.Now()}, cancel: cancel}
		m.hosts[t.Addr] = h
		m.order = append(m.order, t.Addr)
		added = append(added, t.Addr)
		// Stagger start times so large lists don't burst all at once
		delay := time.Duration(m.spawnSeq%40) * m.Interval / 40
		m.spawnSeq++
		toSpawn = append(toSpawn, spawn{ctx, t.Addr, delay})
	}
	m.mu.Unlock()
	for _, sp := range toSpawn {
		m.wg.Add(1)
		go m.worker(sp.ctx, sp.addr, sp.delay)
	}
	return added
}

func mergeLabel(cur *string, add string) {
	if add == "" || add == *cur {
		return
	}
	if *cur == "" {
		*cur = add
		return
	}
	// add may itself be merged; merge piece-wise to avoid duplicates
	// and to tolerate both ", " and "," separators.
	for _, part := range splitLabels(add) {
		found := false
		for _, existing := range splitLabels(*cur) {
			if existing == part {
				found = true
				break
			}
		}
		if !found {
			*cur = *cur + ", " + part
		}
	}
}

func splitLabels(label string) []string {
	var out []string
	for _, p := range strings.Split(label, ",") {
		if q := strings.TrimSpace(p); q != "" {
			out = append(out, q)
		}
	}
	return out
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
		kept := make([]netip.Addr, 0, len(m.order)-len(drop))
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

// Targets returns the monitored addresses, labels and resolver
// provenance, for saving and re-resolving.
func (m *Monitor) Targets() []targets.Target {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]targets.Target, 0, len(m.order))
	for _, a := range m.order {
		out = append(out, targets.Target{Addr: a, Domain: m.hosts[a].Domain, Resolver: m.hosts[a].Resolver, Origin: m.hosts[a].Origin})
	}
	return out
}

// Target returns the monitored target metadata for addr, if present.
func (m *Monitor) Target(addr netip.Addr) (targets.Target, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.hosts[addr]
	if !ok {
		return targets.Target{}, false
	}
	return targets.Target{Addr: h.Addr, Domain: h.Domain, Resolver: h.Resolver, Origin: h.Origin}, true
}

// SetDomain replaces the domain label for addr (if still monitored) and
// reports whether the host was found. Existing resolver provenance,
// state, and history are preserved.
func (m *Monitor) SetDomain(addr netip.Addr, domain string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hosts[addr]
	if !ok {
		return false
	}
	h.Domain = domain
	return true
}

// Count returns how many hosts are monitored.
func (m *Monitor) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.order)
}

// Confirm returns the number of consecutive opposing probes required to
// confirm a state change.
func (m *Monitor) Confirm() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.confirm
}

// SetConfirm changes the confirmation threshold (clamped to 1..10) and
// resets every host's in-progress confirmation counter; confirmed states
// are untouched. It returns the applied value.
func (m *Monitor) SetConfirm(n int) int {
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirm = n
	for _, h := range m.hosts {
		h.CandState, h.CandCount = Pending, 0
	}
	return n
}

// Events returns the retained delta events, newest first, plus how many
// events were dropped (log full) and the latest sequence number (for
// unseen-badge bookkeeping: an event is unseen while Seq > seenSeq).
func (m *Monitor) Events() (events []Event, dropped int, latestSeq uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	events = make([]Event, len(m.events))
	for i, e := range m.events {
		events[len(m.events)-1-i] = e
	}
	return events, m.dropped, m.eventSeq
}

func (m *Monitor) ProberName() string { return m.prober.Name() }

// Pause suspends probing for every host. A probe already in flight still
// completes (and is recorded); no new probes are sent until Resume.
func (m *Monitor) Pause() {
	m.pauseMu.Lock()
	defer m.pauseMu.Unlock()
	if !m.paused {
		m.paused = true
		m.resume = make(chan struct{})
	}
}

// Resume releases every worker blocked in Pause and probing continues.
func (m *Monitor) Resume() {
	m.pauseMu.Lock()
	defer m.pauseMu.Unlock()
	if m.paused {
		m.paused = false
		close(m.resume)
	}
}

// Paused reports whether probing is currently suspended.
func (m *Monitor) Paused() bool {
	m.pauseMu.Lock()
	defer m.pauseMu.Unlock()
	return m.paused
}

// waitResume blocks while the monitor is paused. It returns false if ctx
// was cancelled (monitor stopping or host removed) so the worker exits.
func (m *Monitor) waitResume(ctx context.Context) bool {
	m.pauseMu.Lock()
	if !m.paused {
		m.pauseMu.Unlock()
		return true
	}
	ch := m.resume
	m.pauseMu.Unlock()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// Stop cancels every worker and waits for them to exit.
func (m *Monitor) Stop() {
	m.stop()
	m.wg.Wait()
}

// Wait blocks until all workers have exited (after Stop).
func (m *Monitor) Wait() { m.wg.Wait() }

func (m *Monitor) worker(ctx context.Context, addr netip.Addr, delay time.Duration) {
	defer m.wg.Done()
	if !sleep(ctx, delay) {
		return
	}
	// Probe immediately after the stagger delay, then on a steady ticker
	// so the effective period stays Interval instead of Interval+probe_time.
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	for {
		if !m.waitResume(ctx) {
			return
		}
		rtt, err := m.prober.Probe(ctx, addr, m.Timeout)
		if ctx.Err() != nil {
			return
		}
		m.record(addr, rtt, err)
		select {
		case <-t.C:
		case <-ctx.Done():
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
	now := m.now()
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
	h.History = append(h.History, rtt)
	if len(h.History) > HistoryLen {
		// Copy so the backing array stays capped at HistoryLen instead of
		// growing and being retained by the reslice.
		cp := make([]time.Duration, HistoryLen)
		copy(cp, h.History[len(h.History)-HistoryLen:])
		h.History = cp
	}

	// The first probe sets the baseline silently — no delta, whenever the
	// host joined. Only later confirmed flips are deltas.
	if h.State == Pending {
		h.State = state
		h.Changed = now
		h.CandState, h.CandCount = Pending, 0
		return
	}
	if state == h.State {
		h.CandState, h.CandCount = Pending, 0
		m.checkSettledLocked(h, now)
		return
	}
	// Opposing probe: count consecutive sightings of the candidate state.
	if h.CandState == state {
		h.CandCount++
	} else {
		h.CandState, h.CandCount = state, 1
	}
	if h.CandCount < m.confirm {
		return
	}
	from, prev := h.State, now.Sub(h.Changed)
	h.State, h.Changed = state, now
	h.CandState, h.CandCount = Pending, 0
	h.Changes++
	m.onChangeLocked(h, from, state, prev, now)
}

// onChangeLocked records one confirmed flip: a plain delta event, or flap
// bookkeeping (suppression, tier entry/escalation) when changes come fast.
func (m *Monitor) onChangeLocked(h *host, from, to State, prev time.Duration, now time.Time) {
	// Prune to the slow window, then count both windows.
	kept := h.changeTimes[:0]
	for _, t := range h.changeTimes {
		if t.After(now.Add(-FlapSlowWindow)) {
			kept = append(kept, t)
		}
	}
	h.changeTimes = append(kept, now)
	within := func(w time.Duration) int {
		n := 0
		for _, t := range h.changeTimes {
			if t.After(now.Add(-w)) {
				n++
			}
		}
		return n
	}
	ev := Event{Time: now, Addr: h.Addr, Domain: h.Domain, Origin: h.Origin, From: from, To: to, PrevDuration: prev}
	switch h.Flap {
	case FlapRapid:
		h.flapChanges++ // suppressed
	case FlapSlow:
		h.flapChanges++
		if within(FlapRapidWindow) >= FlapRapidCount {
			h.Flap = FlapRapid
			ev.Kind, ev.Tier = EventFlapRapid, FlapRapid
			m.emitLocked(ev)
		}
	default: // FlapNone
		switch {
		case within(FlapRapidWindow) >= FlapRapidCount:
			h.Flap, h.flapChanges = FlapRapid, 1
			ev.Kind, ev.Tier = EventFlapRapid, FlapRapid
			m.emitLocked(ev)
		case within(FlapSlowWindow) >= FlapSlowCount:
			h.Flap, h.flapChanges = FlapSlow, 1
			ev.Kind, ev.Tier = EventFlapStart, FlapSlow
			m.emitLocked(ev)
		default:
			ev.Kind = EventChange
			m.emitLocked(ev)
		}
	}
}

// checkSettledLocked emits a settled event when a flapping host has kept
// one confirmed state for its tier's settle window.
func (m *Monitor) checkSettledLocked(h *host, now time.Time) {
	if h.Flap == FlapNone {
		return
	}
	settle := FlapSettleSlow
	if h.Flap == FlapRapid {
		settle = FlapSettleRapid
	}
	if now.Sub(h.Changed) < settle {
		return
	}
	ev := Event{Time: now, Addr: h.Addr, Domain: h.Domain, Origin: h.Origin,
		Kind: EventFlapSettled, From: h.State, To: h.State, Tier: h.Flap, Changes: h.flapChanges}
	h.Flap, h.flapChanges = FlapNone, 0
	h.changeTimes = nil // settled: flap history starts over
	m.emitLocked(ev)
}

func (m *Monitor) emitLocked(ev Event) {
	m.eventSeq++
	ev.Seq = m.eventSeq
	m.events = append(m.events, ev)
	if len(m.events) > EventCap {
		excess := len(m.events) - EventCap
		m.dropped += excess
		cp := make([]Event, EventCap)
		copy(cp, m.events[excess:])
		m.events = cp
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
