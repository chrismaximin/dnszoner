package ui

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/dns"
	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

const (
	fValue = iota
	fType
	fHost
	fTTL
	numFields
)

var fieldLabels = [numFields]string{"Value", "Type", "Host", "TTL"}

// field is a text input whose initial content is "selected": typing replaces
// it, while arrow keys start editing it in place.
type field struct {
	old      string
	input    textinput.Model
	pristine bool
}

type editState struct {
	zone   store.Zone
	rec    store.Record // as last synced
	fields [numFields]field
	focus  int
	order  []int // field focus order
	err    string
	types  []string // choices for the type selector

	// creating is set for a record that is not at the provider yet: a new one,
	// or a draft (a new record saved for later, which rec then holds).
	creating, draft bool

	// Confirmation step, once the user asked to save or delete.
	confirming bool
	kind       store.PendingKind
	newData    store.RecordData
	warning    string // a rule the change breaks, which the user confirms past
	applyErr   string
}

func (m *Model) openEdit(r store.Record) tea.Cmd {
	z, ok := m.zoneByID(r.ZoneID)
	if !ok {
		return nil
	}
	start := r.RecordData
	if p, ok := m.pendByKy[keyOf(r.ZoneID, r.RecordData)]; ok && p.Kind == store.PendingUpdate {
		start = p.New // resume the change saved for later
	}
	draft := r.ID == 0 // a pending create
	return m.openEditor(z, r, start, draft, draft)
}

// openNew starts a new record in the zone.
func (m *Model) openNew(z store.Zone) tea.Cmd {
	var ttl int64
	if p := m.provider(z); p != nil {
		ttl = p.AutoTTL()
	}
	r := store.Record{ZoneID: z.ID, RecordData: store.RecordData{Type: "A", Host: "@", TTL: ttl}}
	return m.openEditor(z, r, r.RecordData, true, false)
}

func (m *Model) openEditor(z store.Zone, r store.Record, start store.RecordData, creating, draft bool) tea.Cmd {
	e := &editState{zone: z, rec: r, types: records.Types, creating: creating, draft: draft}
	e.order = []int{fValue, fType, fHost, fTTL}
	if creating {
		e.order = []int{fType, fHost, fValue, fTTL}
	}
	olds := [numFields]string{r.Value, r.Type, hostLabel(r.Host), m.ttlInput(z, r.TTL)}
	starts := [numFields]string{start.Value, start.Type, hostLabel(start.Host), m.ttlInput(z, start.TTL)}
	for i := range e.fields {
		in := textinput.New()
		in.Prompt = ""
		in.CharLimit = 4096
		in.Width = max(20, m.width-30)
		in.SetValue(starts[i])
		e.fields[i] = field{old: olds[i], input: in}
	}
	for _, t := range []string{r.Type, start.Type} {
		if !slices.Contains(e.types, t) {
			e.types = append(slices.Clone(e.types), t)
		}
	}
	m.edit = e
	m.mode = modeEdit
	m.setStatus("", false)
	if creating && !draft {
		return m.focusField(fType)
	}
	return m.focusField(fValue)
}

func (e *editState) typ() string { return e.fields[fType].input.Value() }

// selectType moves the type selector by delta choices.
func (e *editState) selectType(delta int) {
	i := slices.Index(e.types, e.typ())
	n := len(e.types)
	e.fields[fType].input.SetValue(e.types[((i+delta)%n+n)%n])
}

// jumpType selects the next type starting with the letter, so "a" goes A,
// AAAA, A...
func (e *editState) jumpType(letter rune) {
	l := strings.ToUpper(string(letter))
	i := slices.Index(e.types, e.typ())
	for step := 1; step <= len(e.types); step++ {
		t := e.types[(i+step)%len(e.types)]
		if strings.HasPrefix(t, l) {
			e.fields[fType].input.SetValue(t)
			return
		}
	}
}

// valueCheck cleans up and checks the value against the selected type.
// touched is false while neither the value nor the type was changed, since
// existing values are kept as they are.
func (e *editState) valueCheck() (value string, err error, touched bool) {
	raw := e.fields[fValue].input.Value()
	if e.creating && !e.draft && strings.TrimSpace(raw) == "" {
		return "", nil, false // nothing typed yet
	}
	if !(e.creating && !e.draft) && e.typ() == e.rec.Type && strings.TrimSpace(raw) == e.rec.Value {
		return e.rec.Value, nil, false
	}
	v, err := records.Normalize(e.typ(), raw)
	return v, err, true
}

// cnameCheck applies the CNAME rules to the type and host in the form. An
// existing record is only checked once its type or host changes.
func (m *Model) cnameCheck() error {
	e := m.edit
	typ := e.typ()
	host := normalizeHost(strings.TrimSpace(e.fields[fHost].input.Value()), e.zone.Name)
	isNew := e.creating && !e.draft
	if !isNew && typ == e.rec.Type && host == hostLabel(e.rec.Host) {
		return nil
	}
	var others []string
	for _, r := range m.records {
		if r.ZoneID != e.zone.ID || hostLabel(r.Host) != host {
			continue
		}
		if !isNew && keyOf(r.ZoneID, r.RecordData) == keyOf(e.rec.ZoneID, e.rec.RecordData) {
			continue // the record being edited
		}
		others = append(others, r.Type)
	}
	return records.CheckCNAME(typ, host, others)
}

func (m *Model) focusField(i int) tea.Cmd {
	e := m.edit
	e.fields[e.focus].input.Blur()
	e.focus = i
	f := &e.fields[e.focus]
	f.pristine = true
	f.input.CursorEnd()
	return f.input.Focus()
}

// stepField moves the focus delta fields along the focus order.
func (m *Model) stepField(delta int) tea.Cmd {
	e := m.edit
	n := len(e.order)
	return m.focusField(e.order[((slices.Index(e.order, e.focus)+delta)%n+n)%n])
}

func (m *Model) ttlInput(z store.Zone, ttl int64) string {
	if p := m.provider(z); p != nil && p.IsAutoTTL(ttl) {
		return "Auto"
	}
	return strconv.FormatInt(ttl, 10)
}

func (m *Model) updateEdit(msg tea.Msg) tea.Cmd {
	e := m.edit
	k, isKey := msg.(tea.KeyMsg)
	if m.busy != "" {
		return nil // waiting on the provider
	}
	if e.confirming {
		if !isKey {
			return nil
		}
		switch k.String() {
		case "enter", "y":
			return m.applyNow()
		case "l":
			if e.draft && e.kind == store.PendingDelete {
				return nil
			}
			return m.saveForLater()
		case "esc", "n":
			e.confirming = false
			e.applyErr = ""
		}
		return nil
	}

	if isKey {
		switch k.String() {
		case "esc":
			m.closeEdit()
			return nil
		case "tab", "down":
			return m.stepField(1)
		case "shift+tab", "up":
			return m.stepField(-1)
		case "ctrl+t":
			if e.creating && !e.draft {
				return m.openPaste(e.zone)
			}
		case "ctrl+d":
			if e.creating && !e.draft {
				return nil // nothing saved to delete; esc cancels
			}
			e.err = ""
			e.kind = store.PendingDelete
			e.confirming = true
			return nil
		case "enter", "ctrl+s":
			return m.reviewEdit()
		}
	}

	if e.focus == fType {
		if isKey {
			e.err = ""
			switch k.Type {
			case tea.KeyLeft:
				e.selectType(-1)
			case tea.KeyRight, tea.KeySpace:
				e.selectType(1)
			case tea.KeyRunes:
				e.jumpType(k.Runes[0])
			}
		}
		return nil
	}

	f := &e.fields[e.focus]
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
		e.err = ""
	}
	return cmd
}

func (m *Model) closeEdit() {
	m.edit = nil
	m.mode = modeSearch
}

// reviewEdit validates the fields and moves to the confirmation step.
func (m *Model) reviewEdit() tea.Cmd {
	e := m.edit
	d, err := m.parseFields()
	if err != nil {
		e.err = capitalize(err.Error())
		return nil
	}
	e.warning = ""
	if err := m.cnameCheck(); records.IsWarning(err) {
		e.warning = err.Error() // may start with a host name: keep its case
	}
	if e.creating {
		if e.draft && d == e.rec.RecordData {
			e.err = "Nothing changed."
			return nil
		}
		for _, r := range m.records {
			if r.ID != 0 && r.ZoneID == e.zone.ID && r.Type == d.Type && r.Host == d.Host && r.Value == d.Value {
				e.err = "This record already exists."
				return nil
			}
		}
		e.kind = store.PendingCreate
		e.newData = d
		e.confirming = true
		e.applyErr = ""
		return nil
	}
	if d == e.rec.RecordData {
		if _, ok := m.pendByKy[keyOf(e.rec.ZoneID, e.rec.RecordData)]; ok {
			if err := m.opts.Store.DeletePendingFor(e.rec.ZoneID, e.rec.RecordData); err != nil {
				e.err = err.Error()
				return nil
			}
			m.setStatus("Discarded the pending change for "+recordName(e.zone, e.rec.RecordData), false)
			m.closeEdit()
			return m.reloadNow()
		}
		e.err = "Nothing changed."
		return nil
	}
	e.kind = store.PendingUpdate
	e.newData = d
	e.confirming = true
	e.applyErr = ""
	return nil
}

func (m *Model) parseFields() (store.RecordData, error) {
	e := m.edit
	val := func(i int) string { return strings.TrimSpace(e.fields[i].input.Value()) }

	value, err, _ := e.valueCheck()
	if err != nil {
		return store.RecordData{}, fmt.Errorf("invalid %s value: %v", e.typ(), err)
	}
	if value == "" {
		return store.RecordData{}, errors.New("the value cannot be empty")
	}
	if err := m.cnameCheck(); err != nil && !records.IsWarning(err) {
		return store.RecordData{}, err
	}
	d := store.RecordData{
		Type:  e.typ(),
		Host:  normalizeHost(val(fHost), e.zone.Name),
		Value: value,
	}
	ttl := e.rec.TTL
	if t := val(fTTL); t != e.fields[fTTL].old {
		var err error
		if ttl, err = m.parseTTL(t); err != nil {
			return d, err
		}
	}
	d.TTL = ttl
	return d, nil
}

func (m *Model) parseTTL(s string) (int64, error) {
	ttl, auto, err := records.ParseTTL(s)
	if auto {
		return m.autoTTL(m.edit.zone), nil
	}
	return ttl, err
}

// autoTTL is the TTL the zone's provider uses for "auto".
func (m *Model) autoTTL(z store.Zone) int64 {
	if p := m.provider(z); p != nil {
		return p.AutoTTL()
	}
	return 0
}

func normalizeHost(h, zone string) string { return records.NormalizeHost(h, zone) }

func recordName(z store.Zone, d store.RecordData) string {
	if d.Host == "@" || d.Host == "" {
		return z.Name
	}
	return d.Host + "." + z.Name
}

func (m *Model) currentChange() dns.Change {
	e := m.edit
	if e.kind == store.PendingCreate {
		return dns.Change{Kind: e.kind, Old: e.newData, New: e.newData}
	}
	return dns.Change{Kind: e.kind, Old: e.rec.RecordData, New: e.newData}
}

func (m *Model) saveForLater() tea.Cmd {
	e := m.edit
	if _, err := m.savePendingChange(); err != nil {
		e.applyErr = err.Error()
		return nil
	}
	m.closeEdit()
	name := recordName(e.zone, e.rec.RecordData)
	if e.creating {
		name = "new record " + recordName(e.zone, e.newData)
	}
	m.setStatus(fmt.Sprintf("Saved for later: %s (ctrl+l to review)", name), false)
	return m.reloadNow()
}

// savePendingChange stores the edit as a pending change, replacing the draft
// it was made from, if any.
func (m *Model) savePendingChange() (store.Pending, error) {
	e := m.edit
	st := m.opts.Store
	if e.draft {
		if err := st.DeletePendingFor(e.zone.ID, e.rec.RecordData); err != nil {
			return store.Pending{}, err
		}
	}
	ch := m.currentChange()
	p := store.Pending{ZoneID: e.zone.ID, Kind: ch.Kind, Old: ch.Old, New: ch.New}
	return p, st.SavePending(p)
}

// discardDraft drops a new record that was saved for later.
func (m *Model) discardDraft() tea.Cmd {
	e := m.edit
	if err := m.opts.Store.DeletePendingFor(e.zone.ID, e.rec.RecordData); err != nil {
		e.applyErr = err.Error()
		return nil
	}
	m.closeEdit()
	m.setStatus("Discarded the new record "+recordName(e.zone, e.rec.RecordData), false)
	return m.reloadNow()
}

func (m *Model) applyNow() tea.Cmd {
	e := m.edit
	if m.provider(e.zone) == nil {
		e.applyErr = fmt.Sprintf("source %q is not configured", e.zone.Source)
		return nil
	}
	if e.draft {
		if e.kind == store.PendingDelete {
			return m.discardDraft()
		}
		// Replace the draft first, so the change stays pending if it fails.
		p, err := m.savePendingChange()
		if err != nil {
			e.applyErr = err.Error()
			return nil
		}
		m.reloadNow()
		for _, q := range m.pending {
			if keyOf(q.ZoneID, q.Old) == keyOf(p.ZoneID, p.Old) {
				p = q
			}
		}
		e.rec.RecordData, e.draft = p.Old, true
		return m.startApply([]store.Pending{p}, true)
	}
	ch := m.currentChange()
	return m.startApply([]store.Pending{{ZoneID: e.zone.ID, Kind: ch.Kind, Old: ch.Old, New: ch.New}}, true)
}

func (m *Model) reloadNow() tea.Cmd {
	if err := m.reload(); err != nil {
		m.setStatus("Reading cache: "+err.Error(), true)
	}
	return nil
}

func (m *Model) viewEdit() string {
	e := m.edit
	var b strings.Builder
	switch {
	case e.draft:
		b.WriteString(stTitle.Render(" New record") + "  " + stZone.Render(e.zone.Name) + " " + stSource.Render("["+e.zone.Source+"]") +
			"  " + stPending.Render("saved for later, not applied yet") + "\n\n")
	case e.creating:
		b.WriteString(stTitle.Render(" New record") + "  " + stZone.Render(e.zone.Name) + " " + stSource.Render("["+e.zone.Source+"]") + "\n\n")
	default:
		b.WriteString(stTitle.Render(" Edit record") + "  " + stZone.Render(recordName(e.zone, e.rec.RecordData)) +
			" " + stSource.Render("["+e.zone.Source+"]") + "\n\n")
	}

	if e.confirming {
		b.WriteString(m.viewConfirm())
	} else {
		oldW := max(10, m.width/3)
		for _, i := range e.order {
			f := e.fields[i]
			label := stLabel.Render(fieldLabels[i])
			if i == e.focus {
				label = stLabelOn.Render(fieldLabels[i])
			}
			var input string
			switch {
			case i == fType && i == e.focus:
				input = m.viewTypeSelector()
			case i == e.focus && f.pristine:
				input = stHighlight.Render(f.input.Value())
			case i == e.focus:
				input = f.input.View()
			default:
				input = f.input.Value()
			}
			marker := "  "
			if i == e.focus {
				marker = stTitle.Render("› ")
			}
			b.WriteString(" " + marker + label + " " + input + "\n")
			if !e.creating || e.draft {
				prefix := "old: "
				if e.draft {
					prefix = "saved: "
				}
				old := prefix + truncate(f.old, oldW)
				if strings.TrimSpace(f.input.Value()) != f.old {
					old = stPending.Render(old)
				} else {
					old = stMuted.Render(old)
				}
				b.WriteString("           " + old + "\n")
			}
			// The check takes the blank line under an edited value; new
			// records have no old values, so they keep the blank line.
			check := ""
			if i == fValue {
				check = m.viewValueCheck()
			}
			b.WriteString(check)
			if check == "" || e.creating && !e.draft {
				b.WriteString("\n")
			}
		}
		if e.err != "" {
			b.WriteString(m.wrap(e.err, " ", stErr))
		}
	}

	lines := strings.Count(b.String(), "\n")
	for i := lines; i < m.height-2; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	if e.confirming {
		if e.draft && e.kind == store.PendingDelete {
			b.WriteString(helpLine("enter", "discard", "esc", "keep editing"))
		} else {
			b.WriteString(helpLine("enter", "apply now", "l", "save for later", "esc", "keep editing"))
		}
	} else if e.focus == fType {
		b.WriteString(helpLine(append([]string{"←→", "change type", "letter", "jump to type", "tab", "next field", "enter", "save"}, e.deleteHelp()...)...))
	} else {
		b.WriteString(helpLine(append([]string{"type", "replace", "←→", "edit in place", "tab", "next field", "enter", "save"}, e.deleteHelp()...)...))
	}
	return b.String()
}

// deleteHelp is the help for ctrl+d and esc, which depend on whether the
// record exists yet.
func (e *editState) deleteHelp() []string {
	switch {
	case e.draft:
		return []string{"ctrl+d", "discard", "esc", "back"}
	case e.creating:
		return []string{"ctrl+t", "paste many", "esc", "cancel"}
	default:
		return []string{"ctrl+d", "delete", "esc", "back"}
	}
}

// viewTypeSelector lists the types with the selected one highlighted.
func (m *Model) viewTypeSelector() string {
	e := m.edit
	parts := make([]string, len(e.types))
	for i, t := range e.types {
		if t == e.typ() {
			parts[i] = stChoice.Render(" " + t + " ")
		} else {
			parts[i] = stMuted.Render(" " + t + " ")
		}
	}
	return strings.Join(parts, " ")
}

// checkIndent lines the value check up with the field values.
const checkIndent = "           "

// viewValueCheck says whether the value is valid for the selected type, and
// how it will be saved when that differs from what was typed.
func (m *Model) viewValueCheck() string {
	e := m.edit
	v, err, touched := e.valueCheck()
	if err == nil {
		if cerr := m.cnameCheck(); records.IsWarning(cerr) {
			return m.wrap("⚠ "+cerr.Error(), checkIndent, stPending)
		} else if cerr != nil {
			return m.wrap("✗ "+cerr.Error(), checkIndent, stErr)
		}
	}
	switch {
	case !touched:
		return ""
	case err != nil:
		s := "✗ " + err.Error()
		if ex, ok := records.Example[e.typ()]; ok && !strings.Contains(s, ex) {
			s += ", e.g. " + ex
		}
		return m.wrap(s, checkIndent, stErr)
	case v != e.fields[fValue].input.Value():
		return m.wrap("✓ valid "+e.typ()+", saved as "+v, checkIndent, stOK)
	default:
		return m.wrap("✓ valid "+e.typ(), checkIndent, stOK)
	}
}

func (m *Model) viewConfirm() string {
	e := m.edit
	var b strings.Builder
	switch {
	case e.kind == store.PendingCreate:
		b.WriteString(" " + stZone.Render("Add this record?") + "\n\n")
		n := e.newData
		b.WriteString("   " + stOK.Render(fmt.Sprintf("%s  %s  %s  %s", n.Type, hostLabel(n.Host), n.Value, m.ttlLabel(e.zone, n.TTL))) + "\n")
	case e.kind == store.PendingDelete && e.draft:
		b.WriteString(" " + stErr.Render("Discard this new record? It was never applied.") + "\n\n")
		o := e.rec.RecordData
		b.WriteString(fmt.Sprintf("   %s  %s  %s  %s\n", o.Type, hostLabel(o.Host), o.Value, m.ttlLabel(e.zone, o.TTL)))
	case e.kind == store.PendingDelete:
		b.WriteString(" " + stErr.Render("Delete this record?") + "\n\n")
		o := e.rec.RecordData
		b.WriteString(fmt.Sprintf("   %s  %s  %s  %s\n", o.Type, hostLabel(o.Host), o.Value, m.ttlLabel(e.zone, o.TTL)))
	default:
		b.WriteString(" " + stZone.Render("Save this change?") + "\n\n")
		o, n := e.rec.RecordData, e.newData
		rows := [][3]string{
			{"Value", o.Value, n.Value},
			{"Type", o.Type, n.Type},
			{"Host", hostLabel(o.Host), hostLabel(n.Host)},
			{"TTL", m.ttlLabel(e.zone, o.TTL), m.ttlLabel(e.zone, n.TTL)},
		}
		for _, r := range rows {
			if r[1] == r[2] {
				b.WriteString("   " + stLabel.Render(r[0]) + " " + stMuted.Render(r[1]) + "\n")
			} else {
				b.WriteString("   " + stLabel.Render(r[0]) + " " + stDeleted.Render(r[1]) + "  →  " + stOK.Render(r[2]) + "\n")
			}
		}
	}
	if e.warning != "" && e.kind != store.PendingDelete {
		b.WriteString("\n" + m.wrap("⚠ "+e.warning, " ", stPending))
	}
	if e.applyErr != "" {
		b.WriteString("\n" + m.wrap("Error: "+e.applyErr, " ", stErr))
	}
	return b.String()
}
