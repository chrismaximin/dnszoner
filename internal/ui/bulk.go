package ui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

const (
	bFind = iota
	bReplace
	numBulkFields
)

var bulkLabels = [numBulkFields]string{"Find", "Replace"}

var (
	stBulkLabel   = stLabel.Width(9)
	stBulkLabelOn = stLabelOn.Width(9)
)

// bulkItem is one marked record. base is what the record will be before this
// edit: its pending update if it has one, else its synced data.
type bulkItem struct {
	zone store.Zone
	rec  store.Record
	base store.RecordData
}

type bulkState struct {
	items  []bulkItem
	fields [numBulkFields]field
	focus  int
	err    string

	confirming bool
	changes    []store.Pending // computed when confirming
}

// toggleMark marks or unmarks the selected record.
func (m *Model) toggleMark() {
	r, ok := m.selectedRecord()
	if !ok {
		return
	}
	k := keyOf(r.ZoneID, r.RecordData)
	if m.marked[k] {
		delete(m.marked, k)
	} else {
		m.marked[k] = true
	}
}

// toggleMarkAll marks every record in the results, or unmarks them all when
// they already are.
func (m *Model) toggleMarkAll() {
	var keys []pendingKey
	all := true
	for _, l := range m.lines {
		if l.rec < 0 {
			continue
		}
		r := m.records[l.rec]
		k := keyOf(r.ZoneID, r.RecordData)
		keys = append(keys, k)
		all = all && m.marked[k]
	}
	for _, k := range keys {
		if all {
			delete(m.marked, k)
		} else {
			m.marked[k] = true
		}
	}
}

func (m *Model) openBulk() tea.Cmd {
	b := &bulkState{}
	for _, r := range m.records {
		if !m.marked[keyOf(r.ZoneID, r.RecordData)] {
			continue
		}
		z, ok := m.zoneByID(r.ZoneID)
		if !ok {
			continue
		}
		base := r.RecordData
		if p, ok := m.pendByKy[keyOf(r.ZoneID, r.RecordData)]; ok {
			if p.Kind == store.PendingDelete {
				continue
			}
			base = p.New
		}
		b.items = append(b.items, bulkItem{zone: z, rec: r, base: base})
	}
	if len(b.items) == 0 {
		m.setStatus("The marked records are all pending deletion", true)
		return nil
	}
	sort.SliceStable(b.items, func(i, j int) bool {
		if b.items[i].zone.Name != b.items[j].zone.Name {
			return b.items[i].zone.Name < b.items[j].zone.Name
		}
		return b.items[i].base.Host < b.items[j].base.Host
	})

	// When the records share a value, find it and replace it by default.
	common := b.items[0].base.Value
	for _, it := range b.items[1:] {
		if it.base.Value != common {
			common = ""
			break
		}
	}
	for i := range b.fields {
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 4096
		in.Width = max(20, m.width-14)
		in.SetValue(common)
		b.fields[i] = field{old: common, input: in}
	}
	b.fields[bFind].input.Placeholder = "(empty: replace the whole value)"
	m.bulk = b
	m.mode = modeBulk
	m.setStatus("", false)
	return m.focusBulkField(bReplace)
}

func (m *Model) focusBulkField(i int) tea.Cmd {
	b := m.bulk
	b.fields[b.focus].input.Blur()
	b.focus = (i + numBulkFields) % numBulkFields
	f := &b.fields[b.focus]
	f.pristine = true
	f.input.CursorEnd()
	return f.input.Focus()
}

// newValue is the item's value after the find/replace.
func (b *bulkState) newValue(it bulkItem) string {
	find := b.fields[bFind].input.Value()
	repl := strings.TrimSpace(b.fields[bReplace].input.Value())
	if find == "" {
		return repl
	}
	return strings.ReplaceAll(it.base.Value, find, repl)
}

// check returns the item's cleaned up new value, whether it changes, and
// whether it is valid for the record type.
func (b *bulkState) check(it bulkItem) (value string, changed bool, err error) {
	v := b.newValue(it)
	if strings.TrimSpace(v) == it.base.Value {
		return it.base.Value, false, nil
	}
	v, err = records.Normalize(it.base.Type, v)
	return v, v != it.base.Value, err
}

func (m *Model) updateBulk(msg tea.Msg) tea.Cmd {
	b := m.bulk
	k, isKey := msg.(tea.KeyMsg)
	if m.busy != "" {
		return nil
	}
	if b.confirming {
		if !isKey {
			return nil
		}
		switch k.String() {
		case "enter", "y":
			return m.finishBulk(true)
		case "l":
			return m.finishBulk(false)
		case "esc", "n":
			b.confirming = false
		}
		return nil
	}

	if isKey {
		switch k.String() {
		case "esc":
			m.bulk = nil
			m.mode = modeSearch
			return nil
		case "tab", "shift+tab", "up", "down":
			return m.focusBulkField(b.focus + 1)
		case "enter", "ctrl+s":
			return m.reviewBulk()
		}
	}

	f := &b.fields[b.focus]
	if isKey && f.pristine {
		switch k.Type {
		case tea.KeyRunes, tea.KeySpace:
			f.input.SetValue("")
			f.pristine = false
		case tea.KeyBackspace, tea.KeyDelete, tea.KeyCtrlU, tea.KeyCtrlW:
			f.input.SetValue("")
			f.pristine = false
			return nil
		case tea.KeyLeft, tea.KeyRight, tea.KeyHome, tea.KeyEnd, tea.KeyCtrlA, tea.KeyCtrlE:
			f.pristine = false
		}
	}
	var cmd tea.Cmd
	f.input, cmd = f.input.Update(msg)
	if isKey {
		b.err = ""
	}
	return cmd
}

// reviewBulk validates every changed record and moves to the confirmation.
func (m *Model) reviewBulk() tea.Cmd {
	b := m.bulk
	b.changes = b.changes[:0]
	invalid, firstErr := 0, ""
	for _, it := range b.items {
		v, changed, err := b.check(it)
		if err != nil {
			if invalid++; firstErr == "" {
				firstErr = fmt.Sprintf("%s (%s): %v", recordName(it.zone, it.base), it.base.Type, err)
			}
			continue
		}
		if !changed {
			continue
		}
		d := it.base
		d.Value = v
		b.changes = append(b.changes, store.Pending{ZoneID: it.zone.ID, Kind: store.PendingUpdate, Old: it.rec.RecordData, New: d})
	}
	if invalid > 0 {
		b.err = fmt.Sprintf("%d new values are invalid, e.g. %s", invalid, firstErr)
		if invalid == 1 {
			b.err = "Invalid new value for " + firstErr
		}
		return nil
	}
	if len(b.changes) == 0 {
		b.err = "Nothing changed."
		return nil
	}
	b.confirming = true
	return nil
}

// finishBulk saves the changes as pending, then applies them unless they are
// meant for later.
func (m *Model) finishBulk(applyNow bool) tea.Cmd {
	b := m.bulk
	if err := m.opts.Store.SavePendings(b.changes); err != nil {
		b.err = err.Error()
		b.confirming = false
		return nil
	}
	keys := map[pendingKey]bool{}
	for _, p := range b.changes {
		keys[keyOf(p.ZoneID, p.Old)] = true
	}
	for _, it := range b.items {
		delete(m.marked, keyOf(it.rec.ZoneID, it.rec.RecordData))
	}
	n := len(b.changes)
	m.bulk = nil
	m.mode = modeSearch
	m.reloadNow()
	if !applyNow {
		m.setStatus(fmt.Sprintf("Saved %d changes for later (ctrl+l to review)", n), false)
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

func (m *Model) viewBulk() string {
	b := m.bulk
	var s strings.Builder
	zones := map[int64]bool{}
	for _, it := range b.items {
		zones[it.zone.ID] = true
	}
	s.WriteString(stTitle.Render(" Bulk edit") + "  " + stZone.Render(fmt.Sprintf("%d records in %d domains", len(b.items), len(zones))) + "\n\n")

	if b.confirming {
		s.WriteString(" " + stZone.Render(fmt.Sprintf("Save %d changes?", len(b.changes))) + "\n\n")
	} else {
		for i, f := range b.fields {
			label := stBulkLabel.Render(bulkLabels[i])
			marker := "  "
			var input string
			switch {
			case i == b.focus && f.pristine && f.input.Value() != "":
				input = stHighlight.Render(f.input.Value())
			case i == b.focus:
				input = f.input.View()
			case f.input.Value() == "":
				input = stMuted.Render(f.input.Placeholder)
			default:
				input = f.input.Value()
			}
			if i == b.focus {
				label = stBulkLabelOn.Render(bulkLabels[i])
				marker = stTitle.Render("› ")
			}
			s.WriteString(" " + marker + label + " " + input + "\n")
		}
		s.WriteString("\n")
	}

	// Preview: each record's value before and after.
	used := strings.Count(s.String(), "\n")
	room := max(1, m.height-used-4)
	nameW := min(40, max(16, m.width/3))
	valW := max(8, (m.width-nameW-colType-12)/2)
	for i, it := range b.items {
		if i == room-1 && len(b.items) > room {
			s.WriteString(stMuted.Render(fmt.Sprintf("   … and %d more", len(b.items)-i)) + "\n")
			break
		}
		name := pad(recordName(it.zone, it.base), nameW) + " " + pad(it.base.Type, colType) + " "
		v, changed, err := b.check(it)
		switch {
		case err != nil:
			s.WriteString("   " + name + stDeleted.Render(truncate(it.base.Value, valW)) + "  →  " + stErr.Render(truncate("✗ "+err.Error(), valW)) + "\n")
		case !changed:
			s.WriteString("   " + stMuted.Render(name+truncate(it.base.Value, valW)+"  (unchanged)") + "\n")
		default:
			s.WriteString("   " + name + stDeleted.Render(truncate(it.base.Value, valW)) + "  →  " + stOK.Render(truncate(v, valW)) + "\n")
		}
	}
	if b.err != "" {
		s.WriteString("\n" + m.wrap(b.err, " ", stErr))
	}

	for i := strings.Count(s.String(), "\n"); i < m.height-2; i++ {
		s.WriteString("\n")
	}
	s.WriteString(m.statusLine() + "\n")
	if b.confirming {
		s.WriteString(helpLine("enter", "apply all now", "l", "save for later", "esc", "keep editing"))
	} else {
		s.WriteString(helpLine("type", "replace", "tab", "find/replace", "enter", "save", "esc", "back"))
	}
	return s.String()
}
