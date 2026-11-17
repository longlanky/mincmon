// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"context"
	"net/netip"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mincmon/internal/monitor"
	"mincmon/internal/targets"
)

type fake struct{}

func (fake) Probe(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
	return time.Millisecond, nil
}
func (fake) Name() string { return "fake" }
func (fake) Close() error { return nil }

func status(addr, origin string, st monitor.State) monitor.Status {
	return monitor.Status{Addr: netip.MustParseAddr(addr), Origin: origin, State: st}
}

func TestBuildDeltas(t *testing.T) {
	m := Model{expanded: map[string]bool{"10.0.0.0/24": true}}
	m.all = []monitor.Status{
		status("10.0.0.2", "10.0.0.0/24", monitor.Down),
		status("10.0.0.1", "10.0.0.0/24", monitor.Up),
		status("192.0.2.1", "", monitor.Up),
	}
	m.events = []monitor.Event{{Seq: 1, Addr: netip.MustParseAddr("10.0.0.2"),
		Kind: monitor.EventChange, From: monitor.Up, To: monitor.Down, Time: time.Now()}}
	m.buildDeltas()
	// Event, cluster header, expanded hosts sorted by IP, flat host.
	if len(m.drows) != 5 {
		t.Fatalf("drows = %+v", m.drows)
	}
	if m.drows[0].kind != rowEvent || m.drows[1].kind != rowCluster ||
		m.drows[2].status.Addr.String() != "10.0.0.1" || m.drows[3].status.Addr.String() != "10.0.0.2" ||
		m.drows[4].kind != rowFlat {
		t.Fatalf("drows order = %+v", m.drows)
	}
	if c := m.drows[1]; c.up != 1 || c.down != 1 || c.pend != 0 {
		t.Fatalf("cluster counts = %+v", c)
	}
	// Cursor on the event selects its host for delete/detail.
	m.view = viewDeltas
	m.cursor = 0
	m.clamp()
	if m.selAddr.String() != "10.0.0.2" {
		t.Fatalf("event selAddr = %v", m.selAddr)
	}
	// Cursor on the cluster header selects nothing.
	m.cursor = 1
	m.clamp()
	if m.selAddr.IsValid() {
		t.Fatalf("cluster selAddr = %v", m.selAddr)
	}
}

func TestEnterDeltaAndTabCycle(t *testing.T) {
	mon := monitor.New(fake{}, time.Hour, time.Second)
	defer mon.Stop()
	addr := netip.MustParseAddr("10.0.0.1")
	mon.Add([]targets.Target{{Addr: addr}})
	m := Model{mon: mon, expanded: map[string]bool{}, width: 100, height: 30,
		view: viewDeltas, drows: []deltaRow{{kind: rowEvent, ev: monitor.Event{Addr: addr}}}}
	m.enterDelta()
	if m.view != viewAll || m.selAddr != addr || len(m.rows) != 1 {
		t.Fatalf("enter jump: view=%v sel=%v rows=%v", m.view, m.selAddr, m.rows)
	}
	// Default view is All; Tab cycles all -> up -> down -> deltas -> all.
	var def Model
	if def.view != viewAll {
		t.Fatalf("default view = %v, want viewAll", def.view)
	}
	wantCycle := []viewMode{viewUp, viewDown, viewDeltas, viewAll}
	var cur tea.Model = m
	for _, want := range wantCycle {
		cur, _ = cur.(Model).updateNormal(tea.KeyMsg{Type: tea.KeyTab})
		if got := cur.(Model).view; got != want {
			t.Fatalf("tab view = %v, want %v", got, want)
		}
	}
	// Shift+Tab from All wraps back to Deltas.
	cur, _ = cur.(Model).updateNormal(tea.KeyMsg{Type: tea.KeyShiftTab})
	if got := cur.(Model).view; got != viewDeltas {
		t.Fatalf("shift+tab view = %v, want viewDeltas", got)
	}
}

func TestPTRKey(t *testing.T) {
	mon := monitor.New(fake{}, time.Hour, time.Second)
	defer mon.Stop()

	// No selected host -> immediate message without starting busy flow.
	empty := Model{mon: mon, expanded: map[string]bool{}, width: 100, height: 30}
	out, cmd := empty.updateNormal(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	got := out.(Model)
	if got.busy || cmd != nil || got.message != "No host selected." {
		t.Fatalf("empty ptr key: busy=%v cmd=%v msg=%q", got.busy, cmd, got.message)
	}

	// Selected host -> starts busy flow.
	addr := netip.MustParseAddr("10.0.0.1")
	mon.Add([]targets.Target{{Addr: addr}})
	m := Model{mon: mon, prompter: &prompter{}, expanded: map[string]bool{}, width: 100, height: 30}
	m.refresh()
	out, cmd = m.updateNormal(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	got = out.(Model)
	if !got.busy || cmd == nil {
		t.Fatalf("selected ptr key: busy=%v cmd=%v", got.busy, cmd)
	}
}
