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
const Version = "4.0"

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

type Model struct {
	mon      *monitor.Monitor
	prompter *prompter
	dns      app.DNSOptions
	hint     string

	all      []monitor.Status // latest snapshot
	rows     []monitor.Status // filtered + sorted view
	cursor   int
	offset   int
	selAddr  netip.Addr
	sortKey  sortKey
	sortDesc bool

	filter      string
	filtering   bool
	filterInput textinput.Model

	busy    bool
	prompt  *promptReq
	input   textinput.Model
	notes   []string
	message string

	width, height int
}

// Run starts the TUI and blocks until the user quits.
func Run(mon *monitor.Monitor, dns app.DNSOptions, hint string) error {
	in := textinput.New()
	in.Prompt = "> "
	fi := textinput.New()
	fi.Prompt = "/"
	m := Model{
		mon: mon, prompter: &prompter{}, dns: dns, hint: hint,
		input: in, filterInput: fi, width: 100, height: 30,
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

// run starts an app flow in the background; its prompts arrive as
// promptReq messages and its result as actionDone.
func (m *Model) run(f func(app.Prompter) string) tea.Cmd {
	m.busy = true
	m.notes = nil
	m.message = ""
	p := m.prompter
	return func() tea.Msg { return actionDone(f(p)) }
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
		m.prompt = &msg
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
			m.mon.Stop()
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
	switch key {
	case "q", "e":
		m.mon.Stop()
		return m, tea.Quit
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
		m.move(-len(m.rows))
		return m, nil
	case "end", "G":
		m.move(len(m.rows))
		return m, nil
	}

	m.message = "" // any other key dismisses the previous message
	if m.busy {
		return m, nil
	}
	mon, dns := m.mon, m.dns
	switch key {
	case "a":
		return m, m.run(func(p app.Prompter) string { return app.Add(mon, p, dns) })
	case "r":
		sel := m.selAddr
		return m, m.run(func(p app.Prompter) string { return app.Remove(mon, p, sel) })
	case "d", "delete":
		if m.selAddr.IsValid() {
			m.message = app.RemoveMatching(mon, m.selAddr.String())
			m.refresh()
		}
	case "s":
		return m, m.run(func(p app.Prompter) string { return app.Save(mon, p) })
	case "l":
		return m, m.run(func(p app.Prompter) string { return app.Load(mon, p) })
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

func (m *Model) refresh() {
	m.all = m.mon.Snapshot()
	f := strings.ToLower(m.filter)
	rows := m.rows[:0]
	for _, s := range m.all {
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
	// Keep the same host selected across re-sorts and removals
	for i, s := range m.rows {
		if s.Addr == m.selAddr {
			m.cursor = i
			break
		}
	}
	m.clamp()
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
		return func(a, b monitor.Status) bool { return a.Loss() > b.Loss() }
	}
	return func(a, b monitor.Status) bool { return false }
}

func (m *Model) move(delta int) {
	m.cursor += delta
	m.clamp()
}

func (m *Model) clamp() {
	n := len(m.rows)
	if m.cursor >= n {
		m.cursor = n - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if n == 0 {
		m.selAddr = netip.Addr{}
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
			k("a", "add"), k("r", "remove"), k("d", "delete sel"), k("s", "save"),
			k("l", "load"), k("o/O", "sort"), k("/", "filter"), k("↑↓", "select"), k("q", "quit"),
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

type cols struct{ ip, dom, hist int }

func (m Model) layout() cols {
	ipW, domW := len("IP ADDRESS")+1, len("DOMAIN")+1 // +1 for the sort arrow
	for _, s := range m.all {
		ipW = max(ipW, len(s.Addr.String()))
		domW = max(domW, ansi.StringWidth(s.Domain))
	}
	ipW, domW = min(ipW, 39), min(domW, 28)
	// gutter 2 + status 8 + latency 10 + loss 6 + up 7 + down 7 + spaces
	fixed := 2 + 8 + 1 + 1 + 10 + 1 + 6 + 7 + 7 + 1
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
	return cols{ipW, domW, min(hist, monitor.HistoryLen)}
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
	c := m.layout()
	var lines []string

	title := stTitle.Render("mincmon v"+Version) + stDim.Render(fmt.Sprintf(
		"   probe: %s   interval %s   timeout %s   %s",
		m.mon.ProberName(), m.mon.Interval, m.mon.Timeout, time.Now().Format("2006-01-02 15:04:05")))
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
	header := "  " + hdr("STATUS", sortState, 8, false) + " " + hdr("IP ADDRESS", sortIP, c.ip, false) + " " +
		hdr("DOMAIN", sortDomain, c.dom, false) + " " + hdr("LATENCY", sortLatency, 10, true) + " " +
		hdr("LOSS", sortLoss, 6, true) + lpad("UP", 7) + lpad("DOWN", 7)
	if c.hist > 0 {
		header += "  " + pad("HISTORY", c.hist)
	}
	lines = append(lines, stBold.Render(header))
	rule := stDim.Render(strings.Repeat("─", max(0, min(ansi.StringWidth(header), m.width))))
	lines = append(lines, rule)

	vis := m.tableRows()
	end := min(len(m.rows), m.offset+vis)
	for i := m.offset; i < end; i++ {
		s := m.rows[i]
		st, sym := stateStyle(s.State)
		gutter := "  "
		ip := pad(s.Addr.String(), c.ip)
		if i == m.cursor {
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
		line := gutter + st.Render(pad(sym+" "+s.State.String(), 8)) + " " + ip + " " + dom + " " +
			lpad(lat, 10) + " " + lpad(fmt.Sprintf("%.0f%%", s.Loss()*100), 6) +
			lpad(fmt.Sprint(s.OK), 7) + lpad(fmt.Sprint(s.Fail), 7)
		if c.hist > 0 {
			line += "  " + sparkline(s.History, c.hist)
		}
		lines = append(lines, line)
	}
	if len(m.rows) == 0 {
		msg := "  (no hosts — press a to add, l to load)"
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
	if len(m.rows) > vis {
		summary += stDim.Render(fmt.Sprintf("   rows %d–%d of %d", m.offset+1, end, len(m.rows)))
	}
	if m.filter != "" {
		summary += stDim.Render(fmt.Sprintf("   filter: %q", m.filter))
	}
	summary += stDim.Render("   sort: " + sortNames[m.sortKey])
	lines = append(lines, summary)
	lines = append(lines, m.detailLine(), "")
	lines = append(lines, m.bottomLines()...)

	for i, l := range lines {
		lines[i] = ansi.Truncate(l, m.width, "")
	}
	return strings.Join(lines, "\n")
}

func (m Model) detailLine() string {
	if len(m.rows) == 0 || m.cursor >= len(m.rows) {
		return ""
	}
	s := m.rows[m.cursor]
	st, _ := stateStyle(s.State)
	d := stSel.Render(s.Addr.String())
	if s.Domain != "" {
		d += " " + stDomain.Render(s.Domain)
	}
	d += " " + st.Render(s.State.String()) + stDim.Render(fmt.Sprintf(" for %s (since %s)",
		time.Since(s.Changed).Truncate(time.Second), s.Changed.Format("15:04:05")))
	if s.Err != "" {
		d += stDown.Render("   error: " + s.Err)
	}
	return d
}
