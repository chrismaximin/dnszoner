package ui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrismaximin/dnszoner/internal/dns"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// applyJob is one zone's share of the changes being applied.
type applyJob struct {
	zone    store.Zone
	changes []store.Pending // ID 0 for a change made in the edit view
}

// applier tracks changes being applied: one provider call per zone, with as
// many zones in flight per source as the provider allows.
type applier struct {
	queue    []applyJob
	inflight map[string]int // by source
	fromEdit bool

	total, done, failed int // in changes
	firstErr            string
}

type applyDoneMsg struct {
	job  applyJob
	errs []error
}

// startApply applies the changes, which may span any number of zones and
// sources.
func (m *Model) startApply(changes []store.Pending, fromEdit bool) tea.Cmd {
	a := &applier{inflight: map[string]int{}, fromEdit: fromEdit, total: len(changes)}
	byZone := map[int64]int{} // zone ID -> index in queue
	for _, p := range changes {
		z, ok := m.zoneByID(p.ZoneID)
		if !ok {
			a.fail(p, m, "the zone is no longer in the cache")
			continue
		}
		if m.provider(z) == nil {
			a.fail(p, m, fmt.Sprintf("source %q is not configured", z.Source))
			continue
		}
		i, ok := byZone[z.ID]
		if !ok {
			i = len(a.queue)
			byZone[z.ID] = i
			a.queue = append(a.queue, applyJob{zone: z})
		}
		a.queue[i].changes = append(a.queue[i].changes, p)
	}
	m.apply = a
	if len(a.queue) > 0 {
		m.lastJob = a.queue[0]
	}
	return m.dispatchApply()
}

func (a *applier) fail(p store.Pending, m *Model, msg string) {
	a.failed++
	if a.firstErr == "" {
		a.firstErr = msg
	}
	if p.ID != 0 {
		_ = m.opts.Store.SetPendingError(p.ID, msg)
	}
}

// dispatchApply starts every queued job its source has room for, and wraps
// up when nothing is left.
func (m *Model) dispatchApply() tea.Cmd {
	a := m.apply
	var cmds []tea.Cmd
	rest := a.queue[:0]
	for _, job := range a.queue {
		prov := m.provider(job.zone)
		if a.inflight[job.zone.Source] >= max(1, prov.Concurrency()) {
			rest = append(rest, job)
			continue
		}
		a.inflight[job.zone.Source]++
		cmds = append(cmds, applyCmd(prov, job))
	}
	a.queue = rest

	inflight := 0
	for _, n := range a.inflight {
		inflight += n
	}
	if inflight == 0 {
		m.finishApply()
		return nil
	}
	if a.total == 1 {
		m.busy = "Applying to " + m.lastJob.zone.Source + "…"
	} else {
		m.busy = fmt.Sprintf("Applying changes… %d/%d", a.done+a.failed, a.total)
	}
	return tea.Batch(cmds...)
}

func applyCmd(prov dns.Provider, job applyJob) tea.Cmd {
	chs := make([]dns.Change, len(job.changes))
	for i, p := range job.changes {
		chs[i] = dns.Change{Kind: p.Kind, Old: p.Old, New: p.New}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return applyDoneMsg{job: job, errs: prov.Apply(ctx, job.zone, chs)}
	}
}

func (m *Model) handleApplyDone(msg applyDoneMsg) tea.Cmd {
	a := m.apply
	if a == nil {
		return nil
	}
	a.inflight[msg.job.zone.Source]--
	m.lastJob = msg.job
	st := m.opts.Store
	for i, p := range msg.job.changes {
		if err := msg.errs[i]; err != nil {
			a.fail(p, m, err.Error())
			continue
		}
		err := st.ApplyLocal(p.ZoneID, p.Kind, p.Old, p.New)
		if err == nil {
			err = st.DeletePendingFor(p.ZoneID, p.Old)
		}
		if err != nil {
			a.fail(p, m, "applied, but updating the cache failed: "+err.Error())
			continue
		}
		a.done++
	}
	m.dataDirty = true
	return tea.Batch(m.dispatchApply(), m.queueReload())
}

func (m *Model) finishApply() {
	a := m.apply
	m.apply = nil
	m.busy = ""
	switch {
	case a.total == 1 && a.failed == 0:
		p := m.lastJob.changes[0]
		verb := "Saved"
		switch p.Kind {
		case store.PendingDelete:
			verb = "Deleted"
		case store.PendingCreate:
			verb = "Added"
		}
		m.setStatus(fmt.Sprintf("%s %s on %s", verb, recordName(m.lastJob.zone, p.Old), m.lastJob.zone.Source), false)
	case a.total == 1:
		m.setStatus("Failed to apply: "+a.firstErr, true)
	case a.failed == 0:
		m.setStatus(fmt.Sprintf("Applied %d changes", a.total), false)
	default:
		m.setStatus(fmt.Sprintf("Applied %d of %d changes; %d failed, e.g. %s (ctrl+l to review)", a.done, a.total, a.failed, a.firstErr), true)
	}
	if a.fromEdit && m.edit != nil {
		if a.failed == 0 {
			m.closeEdit()
		} else {
			m.edit.applyErr = a.firstErr
		}
	}
	m.reloadNow()
}
