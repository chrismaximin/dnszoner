package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

type pasteStatus int

const (
	pasteAdd pasteStatus = iota
	pasteExists
	pasteDuplicate
	pasteSkipped
	pasteInvalid
)

// pasteItem is a line of the pasted text that looks like a record.
type pasteItem struct {
	p      records.Parsed
	data   store.RecordData
	status pasteStatus
	msg    string
	warn   bool // added despite msg
}

type pasteState struct {
	zone    store.Zone
	area    textarea.Model
	text    string // last analyzed
	items   []pasteItem
	ignored int // lines that aren't records: blanks, headers, domain names

	confirming bool
	err        string
}

func (m *Model) openPaste(z store.Zone) tea.Cmd {
	ta := textarea.New()
	ta.Placeholder = "Paste records here, one per line…"
	ta.CharLimit = 0
	ta.ShowLineNumbers = true
	ta.Prompt = " "
	ta.SetWidth(max(20, m.width-2))
	ta.SetHeight(m.pasteAreaHeight())
	m.paste = &pasteState{zone: z, area: ta}
	m.mode = modePaste
	m.edit = nil
	m.setStatus("", false)
	return m.paste.area.Focus()
}

func (m *Model) pasteAreaHeight() int { return min(12, max(3, m.height/3)) }

// analyze reads the pasted text and works out what each record would do.
func (m *Model) analyzePaste() {
	ps := m.paste
	if ps.area.Value() == ps.text && ps.items != nil {
		return
	}
	ps.text = ps.area.Value()
	ps.items, ps.ignored = ps.items[:0], 0
	z := ps.zone

	// Types at each host: what's cached, plus what this paste adds.
	byHost := map[string][]string{}
	have := map[pendingKey]bool{}
	for _, r := range m.records {
		if r.ZoneID == z.ID {
			byHost[hostLabel(r.Host)] = append(byHost[hostLabel(r.Host)], r.Type)
			have[keyOf(z.ID, r.RecordData)] = true
		}
	}
	adding := map[pendingKey]bool{}

	for _, p := range records.Parse(ps.text, z.Name) {
		if p.Ignored() {
			if p.Text != "" && !strings.HasPrefix(p.Text, "$") { // not blank or a directive
				ps.ignored++
			}
			continue
		}
		it := pasteItem{p: p}
		switch {
		case p.Err != nil:
			it.status, it.msg = pasteInvalid, p.Err.Error()
		case p.Skip != "":
			it.status, it.msg = pasteSkipped, p.Skip
		default:
			ttl := p.TTL
			if p.AutoTTL {
				ttl = m.autoTTL(z)
			}
			it.data = store.RecordData{Type: p.Type, Host: p.Host, Value: p.Value, TTL: ttl}
			k := keyOf(z.ID, it.data)
			switch {
			case have[k]:
				it.status, it.msg = pasteExists, "already in "+z.Name
			case adding[k]:
				it.status, it.msg = pasteDuplicate, "repeated in the text"
			default:
				err := records.CheckCNAME(p.Type, p.Host, byHost[p.Host])
				switch {
				case err != nil && !records.IsWarning(err):
					it.status, it.msg = pasteInvalid, err.Error()
				default:
					if err != nil {
						it.msg, it.warn = err.Error(), true
					}
					adding[k] = true
					byHost[p.Host] = append(byHost[p.Host], p.Type)
				}
			}
		}
		ps.items = append(ps.items, it)
	}
}

func (ps *pasteState) warnings() int {
	n := 0
	for _, it := range ps.items {
		if it.status == pasteAdd && it.warn {
			n++
		}
	}
	return n
}

func (ps *pasteState) count(st pasteStatus) int {
	n := 0
	for _, it := range ps.items {
		if it.status == st {
			n++
		}
	}
	return n
}

func (m *Model) updatePaste(msg tea.Msg) tea.Cmd {
	ps := m.paste
	k, isKey := msg.(tea.KeyMsg)
	if m.busy != "" {
		return nil
	}
	if ps.confirming {
		if !isKey {
			return nil
		}
		switch k.String() {
		case "enter", "y":
			return m.finishPaste(true)
		case "l":
			return m.finishPaste(false)
		case "esc", "n":
			ps.confirming = false
		}
		return nil
	}
	if isKey {
		switch k.String() {
		case "esc":
			m.paste = nil
			m.mode = modeSearch
			return nil
		case "ctrl+s":
			m.analyzePaste()
			if ps.count(pasteAdd) == 0 {
				ps.err = "No new records to add."
				return nil
			}
			ps.err = ""
			ps.confirming = true
			return nil
		}
	}
	var cmd tea.Cmd
	ps.area, cmd = ps.area.Update(msg)
	ps.err = ""
	m.analyzePaste()
	return cmd
}

// finishPaste saves the new records as pending, then applies them unless
// they are meant for later.
func (m *Model) finishPaste(applyNow bool) tea.Cmd {
	ps := m.paste
	var changes []store.Pending
	keys := map[pendingKey]bool{}
	for _, it := range ps.items {
		if it.status == pasteAdd {
			changes = append(changes, store.Pending{ZoneID: ps.zone.ID, Kind: store.PendingCreate, Old: it.data, New: it.data})
			keys[keyOf(ps.zone.ID, it.data)] = true
		}
	}
	if err := m.opts.Store.SavePendings(changes); err != nil {
		ps.err = err.Error()
		ps.confirming = false
		return nil
	}
	m.paste = nil
	m.mode = modeSearch
	m.reloadNow()
	if !applyNow {
		m.setStatus(fmt.Sprintf("Saved %d new records for later (ctrl+l to review)", len(changes)), false)
		return nil
	}
	var queue []store.Pending
	for _, p := range m.pending {
		if keys[keyOf(p.ZoneID, p.Old)] {
			queue = append(queue, p)
		}
	}
	return m.startApply(queue, false)
}

func (m *Model) viewPaste() string {
	ps := m.paste
	m.analyzePaste()
	var b strings.Builder
	b.WriteString(stTitle.Render(" Add records from text") + "  " + stZone.Render(ps.zone.Name) + " " + stSource.Render("["+ps.zone.Source+"]") + "\n")

	if ps.confirming {
		b.WriteString("\n " + stZone.Render(fmt.Sprintf("Add %d records to %s?", ps.count(pasteAdd), ps.zone.Name)) + "\n\n")
	} else {
		b.WriteString(m.wrap("Paste lines copied from dnszoner, a zone file, a table or a provider's web page. Headers and other lines are skipped.", " ", stMuted))
		b.WriteString(ps.area.View() + "\n\n")
		var parts []string
		for _, c := range []struct {
			st    pasteStatus
			label string
		}{{pasteAdd, "to add"}, {pasteExists, "already there"}, {pasteDuplicate, "repeated"}, {pasteSkipped, "skipped"}, {pasteInvalid, "invalid"}} {
			if n := ps.count(c.st); n > 0 {
				part := fmt.Sprintf("%d %s", n, c.label)
				if w := ps.warnings(); c.st == pasteAdd && w > 0 {
					part += stPending.Render(fmt.Sprintf(" (%d with warnings)", w))
				}
				parts = append(parts, part)
			}
		}
		if ps.ignored == 1 {
			parts = append(parts, "1 other line ignored")
		} else if ps.ignored > 1 {
			parts = append(parts, fmt.Sprintf("%d other lines ignored", ps.ignored))
		}
		if len(parts) > 0 {
			b.WriteString(" " + strings.Join(parts, stMuted.Render(" · ")) + "\n")
		}
	}

	// The detected records; in the confirmation, only those to add.
	room := max(1, m.height-strings.Count(b.String(), "\n")-4)
	hostW := min(30, max(8, m.width/5))
	valW := max(10, m.width-colType-hostW-colTTL-12)
	var rows []string
	for _, it := range ps.items {
		if ps.confirming && it.status != pasteAdd {
			continue
		}
		switch it.status {
		case pasteAdd:
			d := it.data
			row := "   " + stOK.Render("+ "+pad(d.Type, colType)+" "+pad(hostLabel(d.Host), hostW)+" "+pad(d.Value, valW)+" "+m.ttlLabel(ps.zone, d.TTL))
			if it.warn {
				row += "\n" + strings.TrimRight(m.wrap(fmt.Sprintf("⚠ line %d: %s", it.p.Line, it.msg), "     ", stPending), "\n")
			}
			rows = append(rows, row)
		case pasteExists, pasteDuplicate, pasteSkipped:
			d := it.p
			rows = append(rows, "   "+stMuted.Render(truncate("= "+pad(d.Type, colType)+" "+pad(hostLabel(d.Host), hostW)+" "+d.Value+"  ("+it.msg+")", max(10, m.width-4))))
		default:
			rows = append(rows, strings.TrimRight(m.wrap(fmt.Sprintf("✗ line %d: %s", it.p.Line, it.msg), "   ", stErr), "\n"))
		}
	}
	used := 0
	for i, r := range rows {
		h := strings.Count(r, "\n") + 1
		if used+h > room-1 && i < len(rows)-1 || used+h > room {
			b.WriteString(stMuted.Render(fmt.Sprintf("   … and %d more", len(rows)-i)) + "\n")
			break
		}
		b.WriteString(r + "\n")
		used += h
	}
	if ps.err != "" {
		b.WriteString("\n" + m.wrap(ps.err, " ", stErr))
	}

	for i := strings.Count(b.String(), "\n"); i < m.height-2; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	if ps.confirming {
		b.WriteString(helpLine("enter", "apply now", "l", "save for later", "esc", "back"))
	} else {
		b.WriteString(helpLine("ctrl+s", "review and add", "esc", "cancel"))
	}
	return b.String()
}
