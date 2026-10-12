package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/store"
)

func (m *Model) updatePending(msg tea.Msg) tea.Cmd {
	k, ok := msg.(tea.KeyMsg)
	if !ok || m.busy != "" && k.String() != "esc" {
		return nil
	}
	switch k.String() {
	case "esc", "q", "ctrl+l":
		m.mode = modeSearch
	case "up", "k", "ctrl+p":
		m.pcursor = max(0, m.pcursor-1)
	case "down", "j", "ctrl+n":
		m.pcursor = min(len(m.pending)-1, m.pcursor+1)
	case "enter", "a":
		if m.pcursor < len(m.pending) {
			return m.startApply([]store.Pending{m.pending[m.pcursor]}, false)
		}
	case "A":
		if len(m.pending) > 0 {
			return m.startApply(append([]store.Pending(nil), m.pending...), false)
		}
	case "e":
		if m.pcursor < len(m.pending) {
			p := m.pending[m.pcursor]
			for _, r := range m.records {
				if r.ZoneID == p.ZoneID && keyOf(r.ZoneID, r.RecordData) == keyOf(p.ZoneID, p.Old) {
					return m.openEdit(r)
				}
			}
			m.setStatus("That record is no longer in the cache; discard the change", true)
		}
	case "x", "d", "delete", "backspace":
		if m.pcursor < len(m.pending) {
			if err := m.opts.Store.DeletePending(m.pending[m.pcursor].ID); err != nil {
				m.setStatus(err.Error(), true)
			} else {
				m.setStatus("Discarded", false)
			}
			return m.reloadNow()
		}
	}
	return nil
}

func (m *Model) viewPending() string {
	var b strings.Builder
	b.WriteString(stTitle.Render(fmt.Sprintf(" Pending changes (%d)", len(m.pending))) + "\n\n")
	if len(m.pending) == 0 {
		b.WriteString(stMuted.Render("  Nothing saved for later.") + "\n")
	}

	// Entries vary in height once errors wrap: scroll back from the cursor as
	// far as fits, then fill the screen from there.
	room := max(1, m.height-5)
	start, used := m.pcursor, 0
	for i := min(m.pcursor, len(m.pending)-1); i >= 0; i-- {
		h := strings.Count(m.viewPendingEntry(i), "\n")
		if used+h > room && i < m.pcursor {
			break
		}
		start, used = i, used+h
	}
	used = 0
	for i := start; i < len(m.pending); i++ {
		entry := m.viewPendingEntry(i)
		if h := strings.Count(entry, "\n"); used+h > room && i > m.pcursor {
			break
		} else {
			used += h
		}
		b.WriteString(entry)
	}

	lines := strings.Count(b.String(), "\n")
	for i := lines; i < m.height-2; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	b.WriteString(helpLine("enter", "apply", "A", "apply all", "e", "edit", "x", "discard", "esc", "back"))
	return b.String()
}

// viewPendingEntry renders one pending change, ending with a blank line.
func (m *Model) viewPendingEntry(i int) string {
	var b strings.Builder
	p := m.pending[i]
	z, _ := m.zoneByID(p.ZoneID)
	marker := "  "
	if i == m.pcursor {
		marker = stTitle.Render("› ")
	}
	head := stZone.Render(recordName(z, p.Old)) + " " + stSource.Render("["+z.Source+"]")
	b.WriteString(" " + marker + head + "\n")
	o := p.Old
	var detail string
	switch p.Kind {
	case store.PendingCreate:
		n := p.New
		detail = stOK.Render("add " + fmt.Sprintf("%s %s %s %s", n.Type, hostLabel(n.Host), n.Value, m.ttlLabel(z, n.TTL)))
	case store.PendingDelete:
		detail = stErr.Render("delete ") + stDeleted.Render(fmt.Sprintf("%s %s %s", o.Type, hostLabel(o.Host), o.Value))
	default:
		n := p.New
		detail = fmt.Sprintf("%s %s %s %s", o.Type, hostLabel(o.Host), o.Value, m.ttlLabel(z, o.TTL)) +
			"  →  " + stOK.Render(fmt.Sprintf("%s %s %s %s", n.Type, hostLabel(n.Host), n.Value, m.ttlLabel(z, n.TTL)))
	}
	b.WriteString("     " + truncate(detail, max(10, m.width-6)) + "\n")
	if p.LastError != "" {
		b.WriteString(m.wrap("last attempt failed: "+p.LastError, "     ", stErr))
	}
	b.WriteString("\n")
	return b.String()
}
