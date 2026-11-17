// Copyright (C) 2026 LongLanky
// SPDX-License-Identifier: GPL-3.0-or-later

// Package tui is the live Bubble Tea interface: a scrollable, sortable,
// filterable host table with per-host RTT sparklines.
package tui

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"mincmon/internal/app"
	"mincmon/internal/monitor"
)

// Version is shown in the title bar.
const Version = "1.3.0"

var (
	stTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	stBold    = lipgloss.NewStyle().Bold(true)
	stDim     = lipgloss.NewStyle().Faint(true)
	stUp      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	stDown    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	stPending = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	stDomain  = lipgloss.NewStyle().Foreground(lipgloss.Color("13"))
	stKey     = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	stMsg     = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	stSel     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
)

// ---------------------------------------------------------------------
//  Prompter bridge: app flows run in a goroutine and block on replies
// ---------------------------------------------------------------------

type promptReq struct {
	text  string
	reply chan promptResp
}

type promptResp struct {
	text string
	err  error
}

type noteMsg string
type actionDone string
type tickMsg time.Time

type prompter struct{ send func(tea.Msg) }

func (p *prompter) Prompt(text string) (string, error) {
	reply := make(chan promptResp, 1)
	p.send(promptReq{text: text, reply: reply})
	r := <-reply
	return r.text, r.err
}

func (p *prompter) Notify(text string) { p.send(noteMsg(text)) }

// ---------------------------------------------------------------------
//  Model
// ---------------------------------------------------------------------

type sortKey int

const (
	sortAdded sortKey = iota
	sortIP
	sortDomain
	sortState
	sortLatency
	sortLoss
	numSortKeys
)

var sortNames = [...]string{"added", "ip", "domain", "state", "latency", "loss"}

// viewMode selects which list the main pane shows. Tab cycles them in
// the order all → up → down → deltas.
type viewMode int

const (
	viewAll viewMode = iota
	viewUp
	viewDown
	viewDeltas
	numViews
)

var viewNames = [...]string{"all", "up", "down", "deltas"}

// deltaRow is one line of the deltas view: a feed event, a subnet
// cluster header, or a host (inside an expanded cluster, or flat).
type rowKind int

const (
	rowEvent rowKind = iota
	rowCluster
	rowHost
	rowFlat
)

type deltaRow struct {
	kind   rowKind
	ev     monitor.Event
	origin string
	status monitor.Status
	up     int
	down   int
	pend   int
	flap   int
}

type Model struct {
	mon      *monitor.Monitor
	prompter *prompter
	dns      app.DNSOptions
	sess     *app.Session
	hint     string

	view      viewMode
	all       []monitor.Status // latest snapshot
	rows      []monitor.Status // filtered + sorted table view
	drows     []deltaRow       // deltas view lines
	events    []monitor.Event  // newest first
	dropped   int
	latestSeq uint64
	seenSeq   uint64
	expanded  map[string]bool
	cursor    int
	offset    int
	selAddr   netip.Addr
	sortKey   sortKey
	sortDesc  bool

	filter      string
	filtering   bool
	filterInput textinput.Model

	busy    bool
	prompt  *promptReq
	help    bool
	input   textinput.Model
	notes   []string
	message string

	width, height int
}

// Run starts the TUI and blocks until the user quits.
func Run(mon *monitor.Monitor, dns app.DNSOptions, sess *app.Session, hint string) error {
	in := textinput.New()
	in.Prompt = "> "
	fi := textinput.New()
	fi.Prompt = "/"
	if sess == nil {
		sess = &app.Session{}
	}
	m := Model{
		mon: mon, prompter: &prompter{}, dns: dns, sess: sess, hint: hint,
		input: in, filterInput: fi, width: 100, height: 30,
		expanded: map[string]bool{},
	}
	m.refresh()
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.prompter.send = p.Send
	_, err := p.Run()
	return err
}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Init() tea.Cmd { return tick() }

// startFlow marks the model busy and returns the command that runs an app
// flow in the background; its prompts arrive as promptReq messages and its
// result as actionDone. It is a free function (not a method mutating m) so
// callers can't hit the value/pointer evaluation-order bug where
// "return m, m.run(...)" copies m before busy=true takes effect.
func startFlow(m *Model, f func(app.Prompter) string) tea.Cmd {
	m.busy = true
	m.notes = nil
	m.message = ""
	p := m.prompter
	return func() tea.Msg { return actionDone(f(p)) }
}

// cancelPrompt unblocks a waiting app-flow goroutine so quitting while a
// prompt is open doesn't leak it (its goroutine would otherwise block on
// the reply channel forever).
func (m *Model) cancelPrompt() {
	if m.prompt != nil {
		m.prompt.reply <- promptResp{err: app.ErrCancelled}
		m.prompt = nil
		m.input.Blur()
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, msg.Width-4)
		m.clamp()
		return m, nil

	case tickMsg:
		m.refresh()
		return m, tick()

	case promptReq:
		if m.prompt != nil {
			// A flow is already prompting (shouldn't happen now that
			// busy blocks concurrent flows); don't leak the old channel.
			// Answer the duplicate with cancel so its goroutine exits.
			msg.reply <- promptResp{err: app.ErrCancelled}
			return m, nil
		}
		// Copy: msg is a loop variable; taking &msg directly aliases it.
		req := msg
		m.prompt = &req
		m.input.SetValue("")
		return m, m.input.Focus()

	case noteMsg:
		m.notes = append(m.notes, string(msg))
		return m, nil

	case actionDone:
		m.busy = false
		m.prompt = nil
		m.message = strings.Join(append(m.notes, string(msg)), "\n")
		m.notes = nil
		m.refresh()
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.cancelPrompt()
			return m, tea.Quit
		}
		if m.prompt != nil {
			return m.updatePrompt(msg)
		}
		if m.filtering {
			return m.updateFilter(msg)
		}
		// Fast typing / key repeat can arrive as one multi-rune message;
		// treat each rune as its own key press.
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
			var cmds []tea.Cmd
			var model tea.Model = m
			for _, r := range msg.Runes {
				var cmd tea.Cmd
				model, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				cmds = append(cmds, cmd)
			}
			return model, tea.Batch(cmds...)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m Model) updatePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		m.prompt.reply <- promptResp{text: m.input.Value()}
		m.prompt = nil
		m.input.Blur()
		return m, nil
	case tea.KeyEsc:
		m.prompt.reply <- promptResp{err: app.ErrCancelled}
		m.prompt = nil
		m.input.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m Model) updateFilter(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		m.filtering = false
		m.filterInput.Blur()
		return m, nil
	case tea.KeyEsc:
		m.filtering = false
		m.filterInput.Blur()
		m.filterInput.SetValue("")
		m.filter = ""
		m.refresh()
		return m, nil
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	m.filter = m.filterInput.Value()
	m.refresh()
	return m, cmd
}

func (m Model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	// Any key closes the help overlay ("?" re-opens it).
	if m.help {
		if key == "?" {
			return m, nil
		}
		m.help = false
		return m, nil
	}
	switch key {
	case "q":
		return m, tea.Quit
	case "tab":
		m.view = (m.view + 1) % numViews
		m.cursor, m.offset = 0, 0
		m.refresh()
		return m, nil
	case "shift+tab":
		m.view = (m.view + numViews - 1) % numViews
		m.cursor, m.offset = 0, 0
		m.refresh()
		return m, nil
	case "enter":
		if m.view == viewDeltas {
			m.enterDelta()
			return m, nil
		}
		return m, nil
	case "up", "k":
		m.move(-1)
		return m, nil
	case "down", "j":
		m.move(1)
		return m, nil
	case "pgup":
		m.move(-m.tableRows())
		return m, nil
	case "pgdown", " ":
		m.move(m.tableRows())
		return m, nil
	case "home", "g":
		m.move(-m.curLen())
		return m, nil
	case "end", "G":
		m.move(m.curLen())
		return m, nil
	}

	m.message = "" // any other key dismisses the previous message
	if m.busy {
		return m, nil
	}
	mon, dns, sess := m.mon, m.dns, m.sess
	switch key {
	case "a":
		cmd := startFlow(&m, func(p app.Prompter) string { return app.Add(mon, p, dns, sess) })
		return m, cmd
	case "c":
		cmd := startFlow(&m, func(p app.Prompter) string { return app.ChangeConfirm(mon, p) })
		return m, cmd
	case "S":
		cmd := startFlow(&m, func(p app.Prompter) string { return app.SaveReport(mon, p) })
		return m, cmd
	case "r":
		sel := m.selAddr
		cmd := startFlow(&m, func(p app.Prompter) string { return app.Remove(mon, p, sel) })
		return m, cmd
	case "d", "delete":
		if m.selAddr.IsValid() {
			m.message = app.RemoveMatching(mon, m.selAddr.String())
			m.refresh()
		}
	case "s":
		cmd := startFlow(&m, func(p app.Prompter) string { return app.Save(mon, p) })
		return m, cmd
	case "l":
		cmd := startFlow(&m, func(p app.Prompter) string { return app.Load(mon, p) })
		return m, cmd
	case "R":
		cmd := startFlow(&m, func(p app.Prompter) string { return app.Reresolve(mon, p, dns) })
		return m, cmd
	case "n":
		if !m.selAddr.IsValid() {
			m.message = "No host selected."
			return m, nil
		}
		sel := m.selAddr
		cmd := startFlow(&m, func(p app.Prompter) string { return app.ResolvePTR(mon, p, dns, sel) })
		return m, cmd
	case "p":
		if mon.Paused() {
			mon.Resume()
			m.message = "Probing resumed."
		} else {
			mon.Pause()
			m.message = "Probing paused (in-flight probe finishes; press p to resume)."
		}
	case "?":
		m.help = true
	case "o":
		m.sortKey = (m.sortKey + 1) % numSortKeys
		m.refresh()
	case "O":
		m.sortDesc = !m.sortDesc
		m.refresh()
	case "/":
		m.filtering = true
		m.filterInput.SetValue(m.filter)
		m.filterInput.CursorEnd()
		return m, m.filterInput.Focus()
	case "esc":
		m.filter = ""
		m.filterInput.SetValue("")
		m.refresh()
	}
	return m, nil
}

// ---------------------------------------------------------------------
//  View state: snapshot, filter, sort, selection
// ---------------------------------------------------------------------

// enterDelta applies Enter in the deltas view: a cluster header toggles
// expanded/collapsed; a feed event jumps to its host in the All view.
func (m *Model) enterDelta() {
	if m.cursor < 0 || m.cursor >= len(m.drows) {
		return
	}
	r := m.drows[m.cursor]
	switch r.kind {
	case rowCluster:
		if m.expanded == nil {
			m.expanded = map[string]bool{}
		}
		m.expanded[r.origin] = !m.expanded[r.origin]
		m.refresh()
	case rowEvent:
		m.selAddr = r.ev.Addr
		m.view = viewAll
		m.cursor, m.offset = 0, 0
		m.refresh()
	}
}

func (m *Model) refresh() {
	m.all = m.mon.Snapshot()
	m.events, m.dropped, m.latestSeq = m.mon.Events()
	if m.view == viewDeltas {
		m.seenSeq = m.latestSeq // viewing the feed marks events seen
		m.buildDeltas()
	}
	f := strings.ToLower(m.filter)
	stateOK := func(s monitor.Status) bool {
		switch m.view {
		case viewUp:
			return s.State == monitor.Up
		case viewDown:
			return s.State == monitor.Down
		}
		return true
	}
	rows := make([]monitor.Status, 0, len(m.all))
	for _, s := range m.all {
		if !stateOK(s) {
			continue
		}
		if f == "" || strings.Contains(s.Addr.String(), f) ||
			strings.Contains(strings.ToLower(s.Domain), f) ||
			strings.Contains(strings.ToLower(s.State.String()), f) {
			rows = append(rows, s)
		}
	}
	m.rows = rows
	if m.sortKey != sortAdded {
		less := m.lessFunc()
		sort.SliceStable(m.rows, func(i, j int) bool {
			if m.sortDesc {
				return less(m.rows[j], m.rows[i])
			}
			return less(m.rows[i], m.rows[j])
		})
	} else if m.sortDesc {
		for i, j := 0, len(m.rows)-1; i < j; i, j = i+1, j-1 {
			m.rows[i], m.rows[j] = m.rows[j], m.rows[i]
		}
	}
	// Keep the same host selected across re-sorts and removals (table
	// views only; the deltas cursor addresses feed/cluster lines).
	if m.view != viewDeltas {
		for i, s := range m.rows {
			if s.Addr == m.selAddr {
				m.cursor = i
				break
			}
		}
	}
	m.clamp()
}

// buildDeltas rebuilds the deltas line list: feed events (newest first),
// then one collapsed cluster per origin in first-seen order (expanded
// clusters inline their hosts, sorted by IP), then origin-less hosts.
func (m *Model) buildDeltas() {
	f := strings.ToLower(m.filter)
	match := func(s string) bool { return f == "" || strings.Contains(strings.ToLower(s), f) }
	var rows []deltaRow
	for _, e := range m.events {
		if match(e.Addr.String()) || match(e.Domain) || match(e.Origin) ||
			match(e.From.String()) || match(e.To.String()) {
			rows = append(rows, deltaRow{kind: rowEvent, ev: e})
		}
	}
	var origins []string
	byOrigin := map[string][]monitor.Status{}
	var flat []monitor.Status
	for _, s := range m.all {
		if s.Origin == "" {
			flat = append(flat, s)
			continue
		}
		if _, ok := byOrigin[s.Origin]; !ok {
			origins = append(origins, s.Origin)
		}
		byOrigin[s.Origin] = append(byOrigin[s.Origin], s)
	}
	for _, o := range origins {
		hosts := byOrigin[o]
		cr := deltaRow{kind: rowCluster, origin: o}
		for _, s := range hosts {
			switch s.State {
			case monitor.Up:
				cr.up++
			case monitor.Down:
				cr.down++
			default:
				cr.pend++
			}
			if s.Flap != monitor.FlapNone {
				cr.flap++
			}
		}
		if match(o) || f == "" {
			rows = append(rows, cr)
		}
		if m.expanded[o] {
			sort.SliceStable(hosts, func(i, j int) bool { return hosts[i].Addr.Less(hosts[j].Addr) })
			for _, s := range hosts {
				if match(s.Addr.String()) || match(s.Domain) {
					rows = append(rows, deltaRow{kind: rowHost, origin: o, status: s})
				}
			}
		}
	}
	for _, s := range flat {
		if match(s.Addr.String()) || match(s.Domain) {
			rows = append(rows, deltaRow{kind: rowFlat, status: s})
		}
	}
	m.drows = rows
}

func (m *Model) lessFunc() func(a, b monitor.Status) bool {
	switch m.sortKey {
	case sortIP:
		return func(a, b monitor.Status) bool { return a.Addr.Less(b.Addr) }
	case sortDomain:
		return func(a, b monitor.Status) bool {
			if (a.Domain == "") != (b.Domain == "") {
				return b.Domain == ""
			}
			return a.Domain < b.Domain
		}
	case sortState:
		rank := map[monitor.State]int{monitor.Down: 0, monitor.Pending: 1, monitor.Up: 2}
		return func(a, b monitor.Status) bool { return rank[a.State] < rank[b.State] }
	case sortLatency:
		lat := func(s monitor.Status) time.Duration {
			if s.State != monitor.Up {
				return time.Duration(1 << 62) // down / pending last
			}
			return s.Latency
		}
		return func(a, b monitor.Status) bool { return lat(a) < lat(b) }
	case sortLoss:
		return func(a, b monitor.Status) bool { return a.Loss() < b.Loss() }
	}
	return func(a, b monitor.Status) bool { return false }
}

func (m *Model) curLen() int {
	if m.view == viewDeltas {
		return len(m.drows)
	}
	return len(m.rows)
}

func (m *Model) move(delta int) {
	m.cursor += delta
	m.clamp()
}

func (m *Model) clamp() {
	n := len(m.rows)
	if m.view == viewDeltas {
		n = len(m.drows)
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n == 0 {
		m.selAddr = netip.Addr{}
	} else if m.view == viewDeltas {
		// Delete/detail act on the host under the cursor when the line
		// names one; a cluster header names none.
		r := m.drows[m.cursor]
		switch r.kind {
		case rowEvent:
			m.selAddr = r.ev.Addr
		case rowHost, rowFlat:
			m.selAddr = r.status.Addr
		default:
			m.selAddr = netip.Addr{}
		}
	} else {
		m.selAddr = m.rows[m.cursor].Addr
	}
	vis := m.tableRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+vis {
		m.offset = m.cursor - vis + 1
	}
	if m.offset > max(0, n-vis) {
		m.offset = max(0, n-vis)
	}
	if m.offset < 0 {
		m.offset = 0
	}
}

// ---------------------------------------------------------------------
//  Rendering
// ---------------------------------------------------------------------

func (m Model) bottomLines() []string {
	var out []string
	switch {
	case m.prompt != nil:
		for _, n := range m.notes {
			out = append(out, stMsg.Render(n))
		}
		lines := strings.Split(strings.TrimRight(m.prompt.text, " "), "\n")
		for _, l := range lines {
			out = append(out, stBold.Render(l))
		}
		out = append(out, m.input.View(), stDim.Render("enter: submit   esc: cancel"))
	case m.busy:
		for _, n := range m.notes {
			out = append(out, stMsg.Render(n))
		}
		out = append(out, stMsg.Render("Working…"))
	case m.filtering:
		out = append(out, m.filterInput.View(), stDim.Render("enter: keep filter   esc: clear"))
	default:
		if m.message != "" {
			for _, l := range strings.Split(m.message, "\n") {
				out = append(out, stMsg.Render(l))
			}
		}
		k := func(key, label string) string { return stKey.Render(key) + " " + label }
		out = append(out, strings.Join([]string{
			k("tab", "view"), k("a", "add"), k("r", "remove"), k("d", "delete sel"), k("s", "save"),
			k("S", "report"), k("l", "load"), k("R", "re-resolve"), k("n", "ptr"), k("p", "pause"), k("c", "confirm"),
			k("o/O", "sort"), k("/", "filter"), k("?", "help"), k("↑↓", "select"), k("q", "quit"),
		}, "  "))
	}
	return out
}

// headerLines is the number of lines above the table rows.
func (m Model) headerLines() int {
	n := 4 // title, blank, column header, rule
	if m.hint != "" {
		n++
	}
	return n
}

// footerLines: rule, summary, selected-host detail, blank.
const footerLines = 4

func (m Model) tableRows() int {
	return max(1, m.height-m.headerLines()-footerLines-len(m.bottomLines()))
}

type cols struct{ ip, dom, hist, status int }

func (m Model) layout() cols {
	ipW, domW := len("IP ADDRESS")+1, len("DOMAIN")+1 // +1 for the sort arrow
	statusW := 8
	for _, s := range m.all {
		ipW = max(ipW, len(s.Addr.String()))
		domW = max(domW, ansi.StringWidth(s.Domain))
		if s.CandCount > 0 || s.Flap != monitor.FlapNone {
			statusW = 19 // "○ DOWN FLAPPING ?2/3" fits
		}
	}
	ipW, domW = min(ipW, 39), min(domW, 28)
	// gutter 2 + status + latency 10 + loss 6 + up 7 + down 7 + spaces
	fixed := 2 + statusW + 1 + 1 + 10 + 1 + 6 + 7 + 7 + 1
	over := fixed + ipW + domW - m.width
	if over > 0 {
		shrink := min(over, domW-6)
		domW -= shrink
		over -= shrink
	}
	if over > 0 {
		ipW = max(ipW-over, 15)
	}
	hist := m.width - fixed - ipW - domW - 1
	if hist < 8 {
		hist = 0
	}
	return cols{ipW, domW, min(hist, monitor.HistoryLen), statusW}
}

// statusText is the STATUS column text: state plus any confirming
// progress ("?2/3") and flap tier markers.
func (m Model) statusText(s monitor.Status) string {
	_, sym := stateStyle(s.State)
	t := sym + " " + s.State.String()
	if s.Flap != monitor.FlapNone {
		t += " " + s.Flap.String()
	}
	if s.CandCount > 0 {
		t += fmt.Sprintf(" ?%d/%d", s.CandCount, m.mon.Confirm())
	}
	return t
}

// hostRow renders one host table line (used by every view).
func (m Model) hostRow(s monitor.Status, selected bool, c cols) string {
	st, _ := stateStyle(s.State)
	gutter := "  "
	ip := pad(s.Addr.String(), c.ip)
	if selected {
		gutter = stSel.Render("▶ ")
		ip = stSel.Render(ip)
	}
	lat := "-"
	if s.State == monitor.Up {
		lat = fmtLatency(s.Latency)
	}
	dom := pad(s.Domain, c.dom)
	if s.Domain != "" {
		dom = stDomain.Render(dom)
	}
	line := gutter + st.Render(pad(m.statusText(s), c.status)) + " " + ip + " " + dom + " " +
		lpad(lat, 10) + " " + lpad(fmt.Sprintf("%.0f%%", s.Loss()*100), 6) +
		lpad(fmt.Sprint(s.OK), 7) + lpad(fmt.Sprint(s.Fail), 7)
	if c.hist > 0 {
		line += "  " + sparkline(s.History, c.hist)
	}
	return line
}

// feedLine renders one delta event for the deltas pane.
func feedLine(e monitor.Event) string {
	host := e.Addr.String()
	if e.Domain != "" {
		host += " " + stDomain.Render(e.Domain)
	}
	t := stDim.Render(e.Time.Format("15:04:05"))
	was := stDim.Render(fmt.Sprintf("(was %s %s)", strings.ToLower(e.From.String()), e.PrevDuration.Truncate(time.Second)))
	switch e.Kind {
	case monitor.EventFlapStart:
		return fmt.Sprintf("%s  %s  %s  %s", t, host, stPending.Render("started FLAPPING"), was)
	case monitor.EventFlapRapid:
		return fmt.Sprintf("%s  %s  %s  %s", t, host, stDown.Render("started RAPID flapping"), was)
	case monitor.EventFlapSettled:
		st, _ := stateStyle(e.To)
		return fmt.Sprintf("%s  %s  %s", t, host, st.Render(fmt.Sprintf("settled %s (was %s, %d changes)",
			e.To, strings.ToLower(e.Tier.String()), e.Changes)))
	default:
		fromSt, _ := stateStyle(e.From)
		toSt, _ := stateStyle(e.To)
		return fmt.Sprintf("%s  %s  %s→%s  %s", t, host, fromSt.Render(e.From.String()), toSt.Render(e.To.String()), was)
	}
}

func pad(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func lpad(s string, w int) string {
	s = ansi.Truncate(s, w, "…")
	return strings.Repeat(" ", max(0, w-ansi.StringWidth(s))) + s
}

func fmtLatency(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 10 {
		return fmt.Sprintf("%.2f ms", ms)
	}
	return fmt.Sprintf("%.1f ms", ms)
}

func stateStyle(s monitor.State) (lipgloss.Style, string) {
	switch s {
	case monitor.Up:
		return stUp, "●"
	case monitor.Down:
		return stDown, "○"
	}
	return stPending, "◌"
}

var bars = []rune("▁▂▃▄▅▆▇█")

func sparkline(h []time.Duration, w int) string {
	if w <= 0 {
		return ""
	}
	if len(h) > w {
		h = h[len(h)-w:]
	}
	var peak time.Duration
	for _, v := range h {
		peak = max(peak, v)
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", w-len(h)))
	for _, v := range h {
		if v < 0 {
			b.WriteString(stDown.Render("×"))
			continue
		}
		lvl := 0
		if peak > 0 {
			lvl = int(float64(v) / float64(peak) * float64(len(bars)-1))
		}
		b.WriteString(stUp.Render(string(bars[lvl])))
	}
	return b.String()
}

func (m Model) View() string {
	if m.help {
		return m.helpView()
	}
	c := m.layout()
	var lines []string

	title := stTitle.Render("mincmon v"+Version) + stDim.Render(fmt.Sprintf(
		"   probe: %s   interval %s   timeout %s   confirm %d   view: %s   %s",
		m.mon.ProberName(), m.mon.Interval, m.mon.Timeout, m.mon.Confirm(), viewNames[m.view],
		time.Now().Format("2006-01-02 15:04:05")))
	if unseen := int(m.latestSeq - m.seenSeq); unseen > 0 && m.view != viewDeltas {
		title += "   " + stDown.Render(fmt.Sprintf("⚠ %d new", unseen))
	}
	if m.mon.Paused() {
		title += "   " + stPending.Render("⏸ PAUSED")
	}
	lines = append(lines, title)
	if m.hint != "" {
		lines = append(lines, stMsg.Render("! "+m.hint))
	}
	lines = append(lines, "")

	hdr := func(name string, key sortKey, w int, right bool) string {
		if m.sortKey == key && key != sortAdded {
			if m.sortDesc {
				name += "▼"
			} else {
				name += "▲"
			}
		}
		if right {
			return lpad(name, w)
		}
		return pad(name, w)
	}
	header := "  " + hdr("STATUS", sortState, c.status, false) + " " + hdr("IP ADDRESS", sortIP, c.ip, false) + " " +
		hdr("DOMAIN", sortDomain, c.dom, false) + " " + hdr("LATENCY", sortLatency, 10, true) + " " +
		hdr("LOSS", sortLoss, 6, true) + lpad("UP", 7) + lpad("DOWN", 7)
	if c.hist > 0 {
		header += "  " + pad("HISTORY", c.hist)
	}
	if m.view == viewDeltas {
		header = "  CHANGES (newest first) — enter: expand subnet / jump to host"
		if m.dropped > 0 {
			header += fmt.Sprintf("   (%d earlier dropped)", m.dropped)
		}
	}
	lines = append(lines, stBold.Render(header))
	rule := stDim.Render(strings.Repeat("─", max(0, min(ansi.StringWidth(header), m.width))))
	lines = append(lines, rule)

	vis := m.tableRows()
	end := min(m.curLen(), m.offset+vis)
	for i := m.offset; i < end; i++ {
		if m.view != viewDeltas {
			lines = append(lines, m.hostRow(m.rows[i], i == m.cursor, c))
			continue
		}
		r := m.drows[i]
		switch r.kind {
		case rowEvent:
			line := "  " + feedLine(r.ev)
			if i == m.cursor {
				line = stSel.Render("▶ ") + feedLine(r.ev)
			}
			lines = append(lines, line)
		case rowCluster:
			mark := "▸"
			if m.expanded[r.origin] {
				mark = "▾"
			}
			line := fmt.Sprintf("  %s %s — %s · %s", mark, r.origin,
				stUp.Render(fmt.Sprintf("%d up", r.up)), stDown.Render(fmt.Sprintf("%d down", r.down)))
			if r.pend > 0 {
				line += fmt.Sprintf(" · %s", stPending.Render(fmt.Sprintf("%d pending", r.pend)))
			}
			if r.flap > 0 {
				line += fmt.Sprintf(" · %s", stDown.Render(fmt.Sprintf("%d flapping", r.flap)))
			}
			if i == m.cursor {
				line = stSel.Render(line)
			}
			lines = append(lines, line)
		default: // rowHost, rowFlat
			lines = append(lines, m.hostRow(r.status, i == m.cursor, c))
		}
	}
	if m.curLen() == 0 {
		msg := "  (no hosts — press a to add, l to load)"
		if m.view == viewDeltas && len(m.all) > 0 {
			msg = fmt.Sprintf("  (no changes yet — baseline set, watching %d host(s))", len(m.all))
		}
		if m.filter != "" {
			msg = fmt.Sprintf("  (no hosts match filter %q — esc to clear)", m.filter)
		}
		lines = append(lines, stDim.Render(msg))
		end++
	}
	for i := end - m.offset; i < vis; i++ {
		lines = append(lines, "")
	}

	lines = append(lines, rule)
	up, down, pending := 0, 0, 0
	for _, s := range m.all {
		switch s.State {
		case monitor.Up:
			up++
		case monitor.Down:
			down++
		default:
			pending++
		}
	}
	summary := stUp.Render(fmt.Sprintf("UP: %d", up)) + "   " + stDown.Render(fmt.Sprintf("DOWN: %d", down)) +
		"   " + stPending.Render(fmt.Sprintf("PENDING: %d", pending)) + fmt.Sprintf("   TOTAL: %d", len(m.all))
	if m.curLen() > vis {
		summary += stDim.Render(fmt.Sprintf("   rows %d–%d of %d", m.offset+1, end, m.curLen()))
	}
	if m.filter != "" {
		summary += stDim.Render(fmt.Sprintf("   filter: %q", m.filter))
	}
	if m.view == viewDeltas {
		summary += stDim.Render(fmt.Sprintf("   events: %d", len(m.events)))
	} else {
		summary += stDim.Render("   sort: " + sortNames[m.sortKey])
	}
	lines = append(lines, summary)
	lines = append(lines, m.detailLine(), "")
	lines = append(lines, m.bottomLines()...)

	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "")
	}
	return strings.Join(lines, "\n")
}

func (m Model) detailLine() string {
	if m.view == viewDeltas {
		if m.cursor < 0 || m.cursor >= len(m.drows) {
			return ""
		}
		r := m.drows[m.cursor]
		switch r.kind {
		case rowCluster:
			return stSel.Render(r.origin) + stDim.Render(fmt.Sprintf(
				" — %d host(s): %d up, %d down, %d pending, %d flapping   (enter expands/collapses)",
				r.up+r.down+r.pend, r.up, r.down, r.pend, r.flap))
		case rowEvent:
			for _, s := range m.all {
				if s.Addr == r.ev.Addr {
					return m.detailFor(s)
				}
			}
			return stDim.Render(r.ev.Line() + "   (host no longer monitored)")
		default:
			return m.detailFor(r.status)
		}
	}
	if len(m.rows) == 0 || m.cursor >= len(m.rows) {
		return ""
	}
	return m.detailFor(m.rows[m.cursor])
}

func (m Model) detailFor(s monitor.Status) string {
	st, _ := stateStyle(s.State)
	d := stSel.Render(s.Addr.String())
	if s.Domain != "" {
		d += " " + stDomain.Render(s.Domain)
		if s.Resolver.IsValid() {
			d += stDim.Render(" via " + s.Resolver.String())
		} else {
			d += stDim.Render(" via system DNS")
		}
	}
	d += " " + st.Render(s.State.String()) + stDim.Render(fmt.Sprintf(" for %s (since %s)",
		time.Since(s.Changed).Truncate(time.Second), s.Changed.Format("15:04:05")))
	if s.Origin != "" {
		d += stDim.Render("   from " + s.Origin)
	}
	if s.Flap != monitor.FlapNone {
		d += stDown.Render("   " + s.Flap.String())
	}
	if s.CandCount > 0 {
		d += stPending.Render(fmt.Sprintf("   confirming %s %d/%d", s.CandState, s.CandCount, m.mon.Confirm()))
	}
	if s.Changes > 0 {
		d += stDim.Render(fmt.Sprintf("   changes: %d", s.Changes))
	}
	if s.Err != "" {
		d += stDown.Render("   error: " + s.Err)
	}
	// RTT statistics over the history window (successful probes only).
	var min, max, sum time.Duration
	cnt := 0
	for _, h := range s.History {
		if h < 0 {
			continue
		}
		if cnt == 0 || h < min {
			min = h
		}
		if cnt == 0 || h > max {
			max = h
		}
		sum += h
		cnt++
	}
	if cnt > 0 {
		d += stDim.Render(fmt.Sprintf("   rtt min/avg/max: %s / %s / %s   jitter %s   sent %d",
			fmtLatency(min), fmtLatency(sum/time.Duration(cnt)), fmtLatency(max),
			fmtLatency(max-min), s.OK+s.Fail))
	}
	return d
}

// helpView is the full-screen key reference (? toggles it).
func (m Model) helpView() string {
	k := func(key, desc string) string {
		return "  " + stKey.Render(pad(key, 12)) + stDim.Render(desc)
	}
	rows := [][2]string{
		{"tab / shift+tab", "cycle view: all → up → down → deltas (deltas is the change feed + subnet clusters)"},
		{"enter", "in deltas: expand/collapse a subnet cluster, or jump from a change to its host"},
		{"a", "add hosts (IPs, ranges, subnets with refinement, domains)"},
		{"r", "remove hosts (blank removes the selected one)"},
		{"d / Del", "delete the host under the cursor"},
		{"s", "save the monitor list to a .ml.txt file (atomic)"},
		{"S", "save the delta report (state-change log) as plain text"},
		{"l", "load a monitor list"},
		{"R", "re-resolve selected domains (blank = all), optionally with a different resolver"},
		{"n", "set/update the selected host's name via reverse DNS (PTR) lookup"},
		{"c", "set how many consecutive probes confirm a state change (default 3)"},
		{"p", "pause / resume probing (in-flight probe finishes)"},
		{"o / O", "cycle sort column / reverse sort direction (table views)"},
		{"/", "filter by IP, domain or state (esc clears)"},
		{"↑/↓, j/k", "move selection"},
		{"pgup/pgdn, space", "page up / page down"},
		{"g / G", "jump to first / last row"},
		{"?", "toggle this help"},
		{"q / Ctrl+C", "quit (workers are stopped after the UI closes)"},
	}
	var b strings.Builder
	b.WriteString(stTitle.Render("mincmon v"+Version) + "  " + stBold.Render("key reference"))
	b.WriteString(strings.Repeat("\n", 2))
	for _, r := range rows {
		b.WriteString(k(r[0], r[1]) + "\n")
	}
	b.WriteString(strings.Repeat("\n", 2))
	b.WriteString(stDim.Render("press any key to close"))
	return b.String()
}
