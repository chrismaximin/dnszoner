package dns

import (
	"context"
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

func (p *Fake) Apply(ctx context.Context, zone store.Zone, ch Change) error {
	select {
	case <-time.After(400 * time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}
	recs, err := p.Store.ZoneRecords(zone.ID)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if sameSet(r, ch.Old) && r.Value == ch.Old.Value {
			return nil
		}
	}
	return ErrConflict
}

func (p *Fake) AutoTTL() int64           { return 0 }
func (p *Fake) IsAutoTTL(ttl int64) bool { return ttl == 0 || ttl == 1799 }
func (p *Fake) Concurrency() int         { return 1 }
