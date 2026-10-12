package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/chrismaximin/dnszoner/internal/store"
)

const (
	colType = 6
	colTTL  = 6
)

func (m *Model) listHeight() int {
	// title, search box, blank, column header ... status, help
	return max(1, m.height-6)
}

func (m *Model) scrollToCursor() {
	h := m.listHeight()
	// Show the zone header above the first record of a group.
	top := m.cursor
	if top > 0 && top <= len(m.lines) && m.lines[top-1].rec < 0 {
		top--
	}
	if top < m.offset {
		m.offset = top
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	m.offset = max(0, min(m.offset, len(m.lines)-h))
	m.offset = max(0, m.offset)
}

func (m *Model) moveCursor(delta int) {
	if len(m.lines) == 0 {
		return
	}
	dir := 1
	if delta < 0 {
		dir = -1
	}
	target := m.cursor
	for steps := 0; steps < abs(delta); steps++ {
		next := target + dir
		for next >= 0 && next < len(m.lines) && !m.selectable(next) {
			next += dir
		}
		if next < 0 || next >= len(m.lines) {
			break
		}
		target = next
	}
	m.cursor = target
	m.scrollToCursor()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func (m *Model) updateSearch(msg tea.Msg) tea.Cmd {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "enter":
			if m.apply != nil {
				m.setStatus("Wait for the changes being applied to finish", true)
				return nil
			}
			if len(m.marked) > 0 {
				return m.openBulk()
			}
			if r, ok := m.selectedRecord(); ok {
				return m.openEdit(r)
			}
			if z, ok := m.selectedZone(); ok {
				return m.openNew(z) // an empty zone
			}
			return nil
		case "ctrl+t":
			if m.apply != nil {
				m.setStatus("Wait for the changes being applied to finish", true)
				return nil
			}
			if z, ok := m.selectedZone(); ok {
				return m.openPaste(z)
			}
			return nil
		case "ctrl+n":
			if m.apply != nil {
				m.setStatus("Wait for the changes being applied to finish", true)
				return nil
			}
			if z, ok := m.selectedZone(); ok {
				return m.openNew(z)
			}
			return nil
		case "tab":
			m.toggleMark()
			m.moveCursor(1)
			return nil
		case "ctrl+a":
			m.toggleMarkAll()
			return nil
		case "up", "ctrl+p", "ctrl+k":
			m.moveCursor(-1)
			return nil
		case "down", "ctrl+j":
			m.moveCursor(1)
			return nil
		case "pgup":
			m.moveCursor(-m.listHeight())
			return nil
		case "pgdown":
			m.moveCursor(m.listHeight())
			return nil
		case "esc":
			switch {
			case m.query.Value() != "":
				m.query.SetValue("")
				m.runSearch(true)
			case len(m.marked) > 0:
				m.marked = map[pendingKey]bool{}
			default:
				return tea.Quit
			}
			return nil
		case "ctrl+r":
			if m.syncEvents != nil {
				m.setStatus("A sync is already running", false)
				return nil
			}
			m.setStatus("", false)
			return m.startSync()
		case "ctrl+o":
			m.openAccounts()
			return nil
		case "ctrl+l":
			m.mode = modePending
			m.pcursor = 0
			return nil
		}
	}
	before := m.query.Value()
	var cmd tea.Cmd
	m.query, cmd = m.query.Update(msg)
	if m.query.Value() != before {
		m.runSearch(true)
	}
	return cmd
}

func (m *Model) viewSearch() string {
	var b strings.Builder
	b.WriteString(stTitle.Render(" dnszoner") + "\n")
	b.WriteString(lipgloss.NewStyle().Foreground(colAccent).Render(" ›") + m.query.View() + "\n\n")

	hostW, valueW := m.columnWidths()
	b.WriteString(stHeader.Render("    "+pad("Type", colType)+" "+pad("Host", hostW)+" "+pad("Value", valueW)+" "+pad("TTL", colTTL)) + "\n")

	h := m.listHeight()
	end := min(len(m.lines), m.offset+h)
	for i := m.offset; i < end; i++ {
		b.WriteString(m.renderLine(i, hostW, valueW) + "\n")
	}
	if len(m.lines) == 0 {
		msg := "  No matches."
		if len(m.zones) == 0 {
			msg = "  No zones cached yet. Press ctrl+o to add an account."
		}
		b.WriteString(stMuted.Render(msg) + "\n")
		end++
	}
	for i := end - m.offset; i < h; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	if len(m.marked) > 0 {
		b.WriteString(helpLine("enter", "bulk edit marked", "tab", "mark", "ctrl+a", "mark all", "ctrl+l", "pending", "esc", "clear"))
	} else {
		b.WriteString(helpLine("enter", "edit", "ctrl+n", "new", "ctrl+t", "paste", "tab", "mark", "ctrl+a", "mark all", "ctrl+l", "pending", "ctrl+o", "accounts", "ctrl+r", "sync", "esc", "clear/quit"))
	}
	return b.String()
}

func (m *Model) columnWidths() (host, value int) {
	avail := max(20, m.width-4-colType-colTTL-3-1)
	host = min(32, avail*2/5)
	return host, avail - host
}

func (m *Model) renderLine(i, hostW, valueW int) string {
	l := m.lines[i]
	g := m.groups[l.group]
	z := m.zones[g.Zone]
	if l.rec < 0 {
		s := " " + stZone.Render(z.Name) + " " + stSource.Render("["+z.Source+"]")
		if z.Note != "" {
			s += " " + stMuted.Render("("+z.Note+")")
		}
		if len(g.Records) == 0 {
			s += " " + stMuted.Render("no records")
			if i == m.cursor {
				return " " + stSelected.Render(ansi.Strip(s[1:])+"  enter: add one")
			}
		}
		return s
	}

	r := m.records[l.rec]
	branch := "├─"
	if i+1 >= len(m.lines) || m.lines[i+1].rec < 0 {
		branch = "└─"
	}
	data := r.RecordData
	marker := " "
	p, hasPending := m.pendByKy[keyOf(r.ZoneID, r.RecordData)]
	value := data.Value
	if hasPending {
		marker = stPending.Render("●")
		if p.Kind == store.PendingUpdate {
			data = p.New
			value = data.Value
		}
	}

	cells := pad(data.Type, colType) + " " + pad(hostLabel(data.Host), hostW) + " " +
		pad(value, valueW) + " " + pad(m.ttlLabel(z, data.TTL), colTTL)

	if r.ID == 0 {
		marker = stPending.Render("+") // a new record saved for later
	}
	if m.marked[keyOf(r.ZoneID, r.RecordData)] {
		marker = stTitle.Render("✓")
	}

	selected := i == m.cursor
	switch {
	case hasPending && p.Kind == store.PendingDelete:
		cells = stDeleted.Render(cells)
	case hasPending:
		cells = stPending.Render(cells)
	}
	prefix := " " + stMuted.Render(branch)
	if selected {
		return prefix + marker + stSelected.Render(ansi.Strip(cells))
	}
	return prefix + marker + cells
}

func hostLabel(h string) string {
	if h == "" {
		return "@"
	}
	return h
}

func (m *Model) ttlLabel(z store.Zone, ttl int64) string {
	if p := m.provider(z); p != nil && p.IsAutoTTL(ttl) {
		return "Auto"
	}
	return formatTTL(ttl)
}

func formatTTL(ttl int64) string {
	switch {
	case ttl >= 86400 && ttl%86400 == 0:
		return fmt.Sprintf("%dd", ttl/86400)
	case ttl >= 3600 && ttl%3600 == 0:
		return fmt.Sprintf("%dh", ttl/3600)
	case ttl >= 60 && ttl%60 == 0:
		return fmt.Sprintf("%dm", ttl/60)
	default:
		return fmt.Sprintf("%d", ttl)
	}
}

// pad truncates or right-pads s to exactly w cells.
func pad(s string, w int) string {
	s = truncate(s, w)
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func truncate(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return ansi.Truncate(s, w, "")
	}
	return ansi.Truncate(s, w, "…")
}

func visibleWidth(s string) int { return ansi.StringWidth(s) }

// wrap word-wraps msg to fit the screen after indent, rendering each line
// with st. Words longer than a line are broken. The result ends with a
// newline.
func (m *Model) wrap(msg, indent string, st lipgloss.Style) string {
	var b strings.Builder
	w := max(10, m.width-ansi.StringWidth(indent)-1)
	for _, line := range strings.Split(ansi.Wrap(msg, w, ""), "\n") {
		b.WriteString(indent + st.Render(strings.TrimRight(line, " ")) + "\n")
	}
	return b.String()
}
