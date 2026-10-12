package dns

import (
	"context"
	"fmt"
	"strings"

	"github.com/chrismaximin/dnszoner/internal/store"
)

// idRecord is a record at a provider that gives each record its own ID.
type idRecord struct {
	ID   string
	Data store.RecordData
}

// idAPI changes a zone's records one at a time, by ID.
type idAPI interface {
	List(ctx context.Context) ([]idRecord, error)
	Create(ctx context.Context, d store.RecordData) error
	Update(ctx context.Context, id string, d store.RecordData) error
	Delete(ctx context.Context, id string) error
}

// applyByID applies a zone's changes record by record: an update or delete
// needs its old record to still be there (else ErrConflict), and a create
// refuses a record that already exists (ErrExists). It returns one error per
// change.
func applyByID(ctx context.Context, api idAPI, chs []Change) []error {
	errs := make([]error, len(chs))
	recs, err := api.List(ctx)
	if err != nil {
		for i := range errs {
			errs[i] = fmt.Errorf("fetching current records: %w", err)
		}
		return errs
	}
	used := map[string]bool{}
	find := func(d store.RecordData) (idRecord, bool) {
		for _, r := range recs {
			if !used[r.ID] && strings.EqualFold(r.Data.Type, d.Type) && r.Data.Host == d.Host && r.Data.Value == d.Value {
				return r, true
			}
		}
		return idRecord{}, false
	}
	for i, ch := range chs {
		switch ch.Kind {
		case store.PendingCreate:
			if _, ok := find(ch.New); ok {
				errs[i] = ErrExists
				continue
			}
			errs[i] = api.Create(ctx, ch.New)
		case store.PendingUpdate, store.PendingDelete:
			old, ok := find(ch.Old)
			if !ok {
				errs[i] = ErrConflict
				continue
			}
			used[old.ID] = true
			if ch.Kind == store.PendingDelete {
				errs[i] = api.Delete(ctx, old.ID)
			} else {
				errs[i] = api.Update(ctx, old.ID, ch.New)
			}
		default:
			errs[i] = fmt.Errorf("unknown change kind %q", ch.Kind)
		}
	}
	return errs
}
