// Package ui is the Bubble Tea interface: search, edit and pending changes.
package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/dns"
	"github.com/chrismaximin/dnszoner/internal/search"
	"github.com/chrismaximin/dnszoner/internal/store"
	"github.com/chrismaximin/dnszoner/internal/syncer"
)

type mode int

const (
	modeSearch mode = iota
	modeEdit
	modePending
	modeBulk
	modeAccounts
)

type Options struct {
	Store       *store.Store
	Providers   map[string]dns.Provider // by source name
	SyncOnStart bool
	Fake        bool // changes to the accounts are not saved

	Config     *config.Config
	ConfigPath string
}

type pendingKey struct {
	zone      int64
	typ, host string
	value     string
}

func keyOf(zone int64, d store.RecordData) pendingKey {
	return pendingKey{zone, d.Type, d.Host, d.Value}
}

type Model struct {
	opts Options
	mode mode

	// Cached data.
	zones    []store.Zone
	zoneIdx  map[int64]int // zone ID -> index in zones
	records  []store.Record
	pending  []store.Pending
	pendByKy map[pendingKey]store.Pending
	index    *search.Index

	// Search view.
	query  textinput.Model
	groups []search.Group
	lines  []line
	cursor int // index into lines, always a record line when there is one
	offset int

	marked   map[pendingKey]bool // records marked for a bulk edit
	bulk     *bulkState
	accounts *accountsState
	edit     *editState
	pcursor  int // pending view cursor

	// Sync state.
	syncEvents   chan syncer.Progress
	syncProgress map[string]syncer.Progress
	syncErrs     []string
	syncErrBySrc map[string]string // first error of the last sync, per source
	reloadQueued bool
	dataDirty    bool

	busy      string   // non-empty while a provider call is in flight
	apply     *applier // changes being applied
	lastJob   applyJob // most recently dispatched or finished
	status    string
	statusErr bool

	width, height int
	loadErr       error
}

// line is one row of the result list: a zone header or a record.
type line struct {
	group int
	rec   int // index into records, -1 for zone headers
}

func New(opts Options) *Model {
	q := textinput.New()
	q.Prompt = "  "
	q.Placeholder = "search domains, hosts and values…"
	q.Focus()
	m := &Model{opts: opts, query: q, syncProgress: map[string]syncer.Progress{}, marked: map[pendingKey]bool{}}
	m.loadErr = m.reload()
	if len(opts.Config.Sources) == 0 && len(m.zones) == 0 {
		m.openAccounts() // first run
	}
	return m
}

func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink}
	if m.opts.SyncOnStart {
		cmds = append(cmds, m.startSync())
	}
	return tea.Batch(cmds...)
}

// reload rebuilds the in-memory data and index from the store, keeping the
// selection on the same record when possible.
func (m *Model) reload() error {
	var selected *pendingKey
	if r, ok := m.selectedRecord(); ok {
		k := keyOf(r.ZoneID, r.RecordData)
		selected = &k
	}

	zones, err := m.opts.Store.Zones()
	if err != nil {
		return err
	}
	records, err := m.opts.Store.Records()
	if err != nil {
		return err
	}
	pending, err := m.opts.Store.Pending()
	if err != nil {
		return err
	}
	m.zones, m.records, m.pending = zones, records, pending
	m.zoneIdx = make(map[int64]int, len(zones))
	sz := make([]search.Zone, len(zones))
	for i, z := range zones {
		m.zoneIdx[z.ID] = i
		sz[i] = search.Zone{Name: z.Name, Source: z.Source}
	}
	sr := make([]search.Record, 0, len(records))
	kept := records[:0]
	for _, r := range records {
		zi, ok := m.zoneIdx[r.ZoneID]
		if !ok {
			continue
		}
		kept = append(kept, r)
		sr = append(sr, search.Record{Zone: zi, Host: r.Host, Value: r.Value, Type: r.Type})
	}
	m.records = kept
	live := make(map[pendingKey]bool, len(kept))
	for _, r := range kept {
		live[keyOf(r.ZoneID, r.RecordData)] = true
	}
	for k := range m.marked {
		if !live[k] {
			delete(m.marked, k)
		}
	}
	m.pendByKy = make(map[pendingKey]store.Pending, len(pending))
	for _, p := range pending {
		m.pendByKy[keyOf(p.ZoneID, p.Old)] = p
	}
	m.index = search.NewIndex(sz, sr)
	m.runSearch(false)
	if selected != nil {
		for i, l := range m.lines {
			if l.rec >= 0 && keyOf(m.records[l.rec].ZoneID, m.records[l.rec].RecordData) == *selected {
				m.cursor = i
				m.scrollToCursor()
				break
			}
		}
	}
	if m.pcursor >= len(m.pending) {
		m.pcursor = max(0, len(m.pending)-1)
	}
	return nil
}

// runSearch recomputes results. With resetCursor, the top result is selected.
func (m *Model) runSearch(resetCursor bool) {
	m.groups = m.index.Search(m.query.Value())
	m.lines = m.lines[:0]
	for gi, g := range m.groups {
		m.lines = append(m.lines, line{group: gi, rec: -1})
		for _, r := range g.Records {
			m.lines = append(m.lines, line{group: gi, rec: r})
		}
	}
	if resetCursor || m.cursor >= len(m.lines) {
		m.cursor, m.offset = 0, 0
	}
	m.cursor = m.nearestRecordLine(m.cursor, 1)
	m.scrollToCursor()
}

func (m *Model) nearestRecordLine(from, dir int) int {
	for i := from; i >= 0 && i < len(m.lines); i += dir {
		if m.lines[i].rec >= 0 {
			return i
		}
	}
	for i := from; i >= 0 && i < len(m.lines); i -= dir {
		if m.lines[i].rec >= 0 {
			return i
		}
	}
	return 0
}

func (m *Model) selectedRecord() (store.Record, bool) {
	if m.cursor < len(m.lines) && m.lines[m.cursor].rec >= 0 {
		return m.records[m.lines[m.cursor].rec], true
	}
	return store.Record{}, false
}

func (m *Model) zoneByID(id int64) (store.Zone, bool) {
	if i, ok := m.zoneIdx[id]; ok {
		return m.zones[i], true
	}
	return store.Zone{}, false
}

func (m *Model) provider(z store.Zone) dns.Provider { return m.opts.Providers[z.Source] }

func (m *Model) setStatus(msg string, isErr bool) {
	m.status, m.statusErr = msg, isErr
}

// --- sync ---

type syncProgressMsg struct {
	p  syncer.Progress
	ok bool // false when the sync is over
}
type reloadTickMsg struct{}

// startSync refreshes the given sources, or all of them when none are given.
func (m *Model) startSync(sources ...string) tea.Cmd {
	provs := m.opts.Providers
	if len(sources) > 0 {
		provs = map[string]dns.Provider{}
		for _, s := range sources {
			if p, ok := m.opts.Providers[s]; ok {
				provs[s] = p
			}
		}
	}
	if m.syncEvents != nil || len(provs) == 0 {
		return nil
	}
	m.syncEvents = make(chan syncer.Progress, 64)
	m.syncProgress = map[string]syncer.Progress{}
	m.syncErrs = nil
	if m.syncErrBySrc == nil {
		m.syncErrBySrc = map[string]string{}
	}
	for s := range provs {
		delete(m.syncErrBySrc, s)
	}
	go syncer.Run(context.Background(), m.opts.Store, provs, m.syncEvents)
	return m.waitSync()
}

func (m *Model) waitSync() tea.Cmd {
	ch := m.syncEvents
	return func() tea.Msg {
		p, ok := <-ch
		return syncProgressMsg{p, ok}
	}
}

func (m *Model) handleSync(msg syncProgressMsg) tea.Cmd {
	if !msg.ok {
		m.syncEvents = nil
		m.dataDirty = true
		if len(m.syncErrs) > 0 {
			m.setStatus(fmt.Sprintf("Sync finished with %d error(s): %s", len(m.syncErrs), m.syncErrs[0]), true)
		} else {
			m.setStatus("Sync complete", false)
		}
		return m.queueReload()
	}
	p := msg.p
	if p.Err != nil {
		label := p.Source
		if p.Zone != "" {
			label += " " + p.Zone
		}
		m.syncErrs = append(m.syncErrs, label+": "+p.Err.Error())
		if _, ok := m.syncErrBySrc[p.Source]; !ok {
			m.syncErrBySrc[p.Source] = strings.TrimPrefix(label+": ", p.Source+": ") + p.Err.Error()
		}
	}
	m.syncProgress[p.Source] = p
	m.dataDirty = true
	return tea.Batch(m.waitSync(), m.queueReload())
}

// queueReload coalesces reloads while a sync streams in zones.
func (m *Model) queueReload() tea.Cmd {
	if m.reloadQueued {
		return nil
	}
	m.reloadQueued = true
	return tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg { return reloadTickMsg{} })
}

func (m *Model) syncSummary() string {
	if m.syncEvents == nil {
		return ""
	}
	done, total := 0, 0
	for _, p := range m.syncProgress {
		done += p.Done
		total += p.Total
	}
	if total == 0 {
		return "syncing…"
	}
	return fmt.Sprintf("syncing %d/%d zones", done, total)
}

// --- update ---

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.query.Width = max(10, msg.Width-6)
		m.scrollToCursor()
		return m, nil
	case syncProgressMsg:
		return m, m.handleSync(msg)
	case reloadTickMsg:
		m.reloadQueued = false
		if m.dataDirty {
			m.dataDirty = false
			if err := m.reload(); err != nil {
				m.setStatus("Reading cache: "+err.Error(), true)
			}
		}
		return m, nil
	case applyDoneMsg:
		return m, m.handleApplyDone(msg)
	case testDoneMsg:
		m.handleTestDone(msg)
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
	}

	switch m.mode {
	case modeEdit:
		return m, m.updateEdit(msg)
	case modePending:
		return m, m.updatePending(msg)
	case modeBulk:
		return m, m.updateBulk(msg)
	case modeAccounts:
		return m, m.updateAccounts(msg)
	default:
		return m, m.updateSearch(msg)
	}
}

func (m *Model) View() string {
	if m.width == 0 {
		return ""
	}
	if m.loadErr != nil {
		return stErr.Render("Could not read the cache: "+m.loadErr.Error()) + "\n"
	}
	var body string
	switch m.mode {
	case modeEdit:
		body = m.viewEdit()
	case modePending:
		body = m.viewPending()
	case modeBulk:
		body = m.viewBulk()
	case modeAccounts:
		body = m.viewAccounts()
	default:
		body = m.viewSearch()
	}
	return body
}

// statusLine renders the bottom bar: status message on the left, counts and
// sync progress on the right.
func (m *Model) statusLine() string {
	left := m.status
	switch {
	case m.busy != "":
		left = stPending.Render(m.busy)
	case m.statusErr:
		left = stErr.Render(left)
	case left != "":
		left = stOK.Render(left)
	}
	var right []string
	if s := m.syncSummary(); s != "" {
		right = append(right, s)
	}
	right = append(right, fmt.Sprintf("%d zones", len(m.zones)), fmt.Sprintf("%d records", len(m.records)))
	if n := len(m.marked); n > 0 {
		right = append(right, stSource.Render(fmt.Sprintf("%d marked", n)))
	}
	if n := len(m.pending); n > 0 {
		right = append(right, stPending.Render(fmt.Sprintf("%d pending", n)))
	}
	if m.opts.Fake {
		right = append(right, stPending.Render("FAKE DATA"))
	}
	r := stMuted.Render(strings.Join(right, " · "))
	gap := m.width - visibleWidth(left) - visibleWidth(r) - 2
	if gap < 1 {
		left = truncate(m.status, max(0, m.width-visibleWidth(r)-4))
		if m.statusErr {
			left = stErr.Render(left)
		}
		gap = max(1, m.width-visibleWidth(left)-visibleWidth(r)-2)
	}
	return " " + left + strings.Repeat(" ", gap) + r
}

func helpLine(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, stKey.Render(pairs[i])+" "+stMuted.Render(pairs[i+1]))
	}
	return " " + strings.Join(parts, stMuted.Render(" · "))
}
