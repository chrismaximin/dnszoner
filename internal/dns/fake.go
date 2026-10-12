package dns

import (
	"context"
	"slices"
	"time"

	"github.com/chrismaximin/dnszoner/internal/store"
)

// Fake is the provider used with --fake: the fake database is the source of
// truth, and changes only take a moment to "apply".
type Fake struct {
	Store  *store.Store
	Source string
}

func (p *Fake) ListZones(ctx context.Context) ([]store.Zone, error) {
	all, err := p.Store.Zones()
	if err != nil {
		return nil, err
	}
	var zones []store.Zone
	for _, z := range all {
		if z.Source == p.Source {
			zones = append(zones, z)
		}
	}
	return zones, nil
}

func (p *Fake) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	return p.Store.ZoneRecords(zone.ID)
}

func (p *Fake) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	errs := make([]error, len(chs))
	select {
	case <-time.After(400 * time.Millisecond):
	case <-ctx.Done():
		for i := range errs {
			errs[i] = ctx.Err()
		}
		return errs
	}
	recs, err := p.Store.ZoneRecords(zone.ID)
	for i, ch := range chs {
		switch {
		case err != nil:
			errs[i] = err
		case !slices.ContainsFunc(recs, func(r store.RecordData) bool { return sameSet(r, ch.Old) && r.Value == ch.Old.Value }):
			errs[i] = ErrConflict
		}
	}
	return errs
}

func (p *Fake) AutoTTL() int64           { return 0 }
func (p *Fake) IsAutoTTL(ttl int64) bool { return ttl == 0 || ttl == 1799 }
func (p *Fake) Concurrency() int         { return 4 }
