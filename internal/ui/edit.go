package ui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/libdns/libdns"

	"github.com/chrismaximin/dnszoner/internal/dns"
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
	err    string

	// Confirmation step, once the user asked to save or delete.
	confirming bool
	kind       store.PendingKind
	newData    store.RecordData
	applyErr   string
}

func (m *Model) openEdit(r store.Record) tea.Cmd {
	z, ok := m.zoneByID(r.ZoneID)
	if !ok {
		return nil
	}
	e := &editState{zone: z, rec: r}
	start := r.RecordData
	if p, ok := m.pendByKy[keyOf(r.ZoneID, r.RecordData)]; ok && p.Kind == store.PendingUpdate {
		start = p.New // resume the change saved for later
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
	m.edit = e
	m.mode = modeEdit
	m.setStatus("", false)
	return m.focusField(fValue)
}

func (m *Model) focusField(i int) tea.Cmd {
	e := m.edit
	e.fields[e.focus].input.Blur()
	e.focus = (i + numFields) % numFields
	f := &e.fields[e.focus]
	f.pristine = true
	f.input.CursorEnd()
	return f.input.Focus()
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
			return m.focusField(e.focus + 1)
		case "shift+tab", "up":
			return m.focusField(e.focus - 1)
		case "ctrl+d":
			e.err = ""
			e.kind = store.PendingDelete
			e.confirming = true
			return nil
		case "enter", "ctrl+s":
			return m.reviewEdit()
		}
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
		e.err = err.Error()
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

	d := store.RecordData{
		Type:  strings.ToUpper(val(fType)),
		Host:  normalizeHost(val(fHost), e.zone.Name),
		Value: val(fValue),
	}
	if d.Type == "" || strings.IndexFunc(d.Type, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) >= 0 {
		return d, fmt.Errorf("invalid type %q", d.Type)
	}
	if d.Value == "" {
		return d, errors.New("value cannot be empty")
	}
	ttl := e.rec.TTL
	if t := val(fTTL); t != e.fields[fTTL].old {
		var err error
		if ttl, err = m.parseTTL(t); err != nil {
			return d, err
		}
	}
	d.TTL = ttl
	// Unchanged TXT values etc. may not round-trip through the parser, so
	// only validate what the user touched.
	if d.Type != e.rec.Type || d.Value != e.rec.Value {
		rr := libdns.RR{Name: d.Host, Type: d.Type, Data: d.Value, TTL: time.Duration(ttl) * time.Second}
		if _, err := rr.Parse(); err != nil {
			return d, fmt.Errorf("invalid %s value: %v", d.Type, err)
		}
	}
	return d, nil
}

func (m *Model) parseTTL(s string) (int64, error) {
	if s == "" || strings.EqualFold(s, "auto") {
		if p := m.provider(m.edit.zone); p != nil {
			return p.AutoTTL(), nil
		}
		return 0, nil
	}
	mult := int64(1)
	switch strings.ToLower(s[len(s)-1:]) {
	case "s":
		s = s[:len(s)-1]
	case "m":
		mult, s = 60, s[:len(s)-1]
	case "h":
		mult, s = 3600, s[:len(s)-1]
	case "d":
		mult, s = 86400, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid TTL %q (seconds, 5m, 1h, 1d or auto)", s)
	}
	return n * mult, nil
}

// normalizeHost turns "www.example.com", "example.com." or "" into the
// zone-relative form ("www", "@").
func normalizeHost(h, zone string) string {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	switch {
	case h == "" || h == zone:
		return "@"
	case strings.HasSuffix(h, "."+zone):
		return strings.TrimSuffix(h, "."+zone)
	}
	return h
}

func recordName(z store.Zone, d store.RecordData) string {
	if d.Host == "@" || d.Host == "" {
		return z.Name
	}
	return d.Host + "." + z.Name
}

func (m *Model) currentChange() dns.Change {
	e := m.edit
	return dns.Change{Kind: e.kind, Old: e.rec.RecordData, New: e.newData}
}

func (m *Model) saveForLater() tea.Cmd {
	e := m.edit
	ch := m.currentChange()
	err := m.opts.Store.SavePending(store.Pending{ZoneID: e.zone.ID, Kind: ch.Kind, Old: ch.Old, New: ch.New})
	if err != nil {
		e.applyErr = err.Error()
		return nil
	}
	m.closeEdit()
	m.setStatus(fmt.Sprintf("Saved for later: %s (ctrl+l to review)", recordName(e.zone, e.rec.RecordData)), false)
	return m.reloadNow()
}

func (m *Model) reloadNow() tea.Cmd {
	if err := m.reload(); err != nil {
		m.setStatus("Reading cache: "+err.Error(), true)
	}
	return nil
}

type applyDoneMsg struct {
	pendingID int64 // 0 when applied from the edit view
	zone      store.Zone
	change    dns.Change
	err       error
	rest      []store.Pending // remaining queue when applying all
}

func (m *Model) applyNow() tea.Cmd {
	e := m.edit
	prov := m.provider(e.zone)
	if prov == nil {
		e.applyErr = fmt.Sprintf("source %q is not configured", e.zone.Source)
		return nil
	}
	m.busy = "Applying to " + e.zone.Source + "…"
	return applyCmd(prov, e.zone, m.currentChange(), 0, nil)
}

func applyCmd(prov dns.Provider, z store.Zone, ch dns.Change, pendingID int64, rest []store.Pending) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		err := prov.Apply(ctx, z, ch)
		return applyDoneMsg{pendingID: pendingID, zone: z, change: ch, err: err, rest: rest}
	}
}

func (m *Model) handleApplyDone(msg applyDoneMsg) tea.Cmd {
	m.busy = ""
	name := recordName(msg.zone, msg.change.Old)
	if msg.err != nil {
		if msg.pendingID != 0 {
			_ = m.opts.Store.SetPendingError(msg.pendingID, msg.err.Error())
		} else if m.edit != nil {
			m.edit.applyErr = msg.err.Error()
		}
		m.setStatus(fmt.Sprintf("Failed to apply %s: %v", name, msg.err), true)
	} else {
		st := m.opts.Store
		err := st.ApplyLocal(msg.zone.ID, msg.change.Kind, msg.change.Old, msg.change.New)
		if err == nil {
			err = st.DeletePendingFor(msg.zone.ID, msg.change.Old)
		}
		if err != nil {
			m.setStatus("Applied, but updating the cache failed: "+err.Error(), true)
		} else {
			verb := "Saved"
			if msg.change.Kind == store.PendingDelete {
				verb = "Deleted"
			}
			m.setStatus(fmt.Sprintf("%s %s on %s", verb, name, msg.zone.Source), false)
		}
		if msg.pendingID == 0 {
			m.closeEdit()
		}
	}
	m.reloadNow()
	if len(msg.rest) > 0 {
		return m.applyPending(msg.rest)
	}
	return nil
}

func (m *Model) viewEdit() string {
	e := m.edit
	var b strings.Builder
	b.WriteString(stTitle.Render(" Edit record") + "  " + stZone.Render(recordName(e.zone, e.rec.RecordData)) +
		" " + stSource.Render("["+e.zone.Source+"]") + "\n\n")

	if e.confirming {
		b.WriteString(m.viewConfirm())
	} else {
		oldW := max(10, m.width/3)
		for i, f := range e.fields {
			label := stLabel.Render(fieldLabels[i])
			if i == e.focus {
				label = stLabelOn.Render(fieldLabels[i])
			}
			var input string
			switch {
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
			old := "old: " + truncate(f.old, oldW)
			if strings.TrimSpace(f.input.Value()) != f.old {
				old = stPending.Render(old)
			} else {
				old = stMuted.Render(old)
			}
			b.WriteString("           " + old + "\n\n")
		}
		if e.err != "" {
			b.WriteString(" " + stErr.Render(e.err) + "\n")
		}
	}

	lines := strings.Count(b.String(), "\n")
	for i := lines; i < m.height-2; i++ {
		b.WriteString("\n")
	}
	b.WriteString(m.statusLine() + "\n")
	if e.confirming {
		b.WriteString(helpLine("enter", "apply now", "l", "save for later", "esc", "keep editing"))
	} else {
		b.WriteString(helpLine("type", "replace", "←→", "edit in place", "tab", "next field", "enter", "save", "ctrl+d", "delete", "esc", "back"))
	}
	return b.String()
}

func (m *Model) viewConfirm() string {
	e := m.edit
	var b strings.Builder
	if e.kind == store.PendingDelete {
		b.WriteString(" " + stErr.Render("Delete this record?") + "\n\n")
		o := e.rec.RecordData
		b.WriteString(fmt.Sprintf("   %s  %s  %s  %s\n", o.Type, hostLabel(o.Host), o.Value, m.ttlLabel(e.zone, o.TTL)))
	} else {
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
	if e.applyErr != "" {
		b.WriteString("\n " + stErr.Render("Error: "+e.applyErr) + "\n")
	}
	return b.String()
}
