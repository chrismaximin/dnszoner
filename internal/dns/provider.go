// Package dns talks to DNS providers: listing zones, fetching records and
// applying single-record changes.
package dns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/libdns/libdns"

	"github.com/chrismaximin/dnszoner/internal/store"
)

// ErrConflict means the record no longer matches what the cache shows, so a
// change was not applied.
var ErrConflict = errors.New("record changed at the provider since the last sync; resync (ctrl+r) and retry")

type Change struct {
	Kind store.PendingKind
	Old  store.RecordData
	New  store.RecordData // ignored for deletes
}

type Provider interface {
	ListZones(ctx context.Context) ([]store.Zone, error)
	GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error)
	Apply(ctx context.Context, zone store.Zone, ch Change) error
	// AutoTTL is the TTL used when the user types "auto".
	AutoTTL() int64
	// IsAutoTTL reports whether a TTL is the provider's automatic value.
	IsAutoTTL(ttl int64) bool
	// Concurrency is how many zones may be fetched at once during a sync.
	Concurrency() int
}

// recordAPI is the subset of libdns used to read and change records.
type recordAPI interface {
	libdns.RecordGetter
	libdns.RecordSetter
	libdns.RecordDeleter
}

func fqdn(zone string) string { return strings.TrimSuffix(zone, ".") + "." }

func fromLibdns(recs []libdns.Record) []store.RecordData {
	out := make([]store.RecordData, 0, len(recs))
	for _, r := range recs {
		rr := r.RR()
		out = append(out, store.RecordData{
			Type:  rr.Type,
			Host:  rr.Name,
			Value: rr.Data,
			TTL:   int64(rr.TTL / time.Second),
		})
	}
	return out
}

func toLibdns(d store.RecordData) libdns.Record {
	rr := libdns.RR{Name: d.Host, Type: d.Type, Data: d.Value, TTL: time.Duration(d.TTL) * time.Second}
	// Typed records convert more faithfully in some providers (e.g. MX
	// preference with Namecheap).
	if parsed, err := rr.Parse(); err == nil {
		return parsed
	}
	return rr
}

func sameSet(a, b store.RecordData) bool {
	return strings.EqualFold(a.Type, b.Type) && a.Host == b.Host
}

// applyLibdns applies a single-record change with libdns semantics, where
// records sharing a (host, type) form one set. It re-reads the zone first and
// refuses to proceed if the old record is gone or changed.
func applyLibdns(ctx context.Context, api recordAPI, zone string, ch Change) error {
	recs, err := api.GetRecords(ctx, fqdn(zone))
	if err != nil {
		return fmt.Errorf("fetching current records: %w", err)
	}
	current := fromLibdns(recs)

	var oldSet []store.RecordData
	found := false
	for _, r := range current {
		if !sameSet(r, ch.Old) {
			continue
		}
		if r.Value == ch.Old.Value {
			if found {
				continue // drop exact duplicates of the edited record
			}
			found = true
		}
		oldSet = append(oldSet, r)
	}
	if !found {
		return ErrConflict
	}

	var others []store.RecordData // old set without the edited record
	for _, r := range oldSet {
		if r.Value != ch.Old.Value {
			others = append(others, r)
		}
	}

	switch ch.Kind {
	case store.PendingDelete:
		_, err = api.DeleteRecords(ctx, fqdn(zone), []libdns.Record{toLibdns(ch.Old)})
		return err

	case store.PendingUpdate:
		n := ch.New
		if sameSet(ch.Old, n) {
			set := []libdns.Record{toLibdns(n)}
			for _, r := range others {
				if r.Value == n.Value {
					continue
				}
				r.TTL = n.TTL
				set = append(set, toLibdns(r))
			}
			_, err = api.SetRecords(ctx, fqdn(zone), set)
			return err
		}

		// Host or type changed: add to the new set, then remove from the old.
		set := []libdns.Record{toLibdns(n)}
		for _, r := range current {
			if sameSet(r, n) && r.Value != n.Value {
				r.TTL = n.TTL
				set = append(set, toLibdns(r))
			}
		}
		if _, err := api.SetRecords(ctx, fqdn(zone), set); err != nil {
			return err
		}
		if len(others) > 0 {
			rest := make([]libdns.Record, 0, len(others))
			for _, r := range others {
				rest = append(rest, toLibdns(r))
			}
			_, err = api.SetRecords(ctx, fqdn(zone), rest)
		} else {
			_, err = api.DeleteRecords(ctx, fqdn(zone), []libdns.Record{toLibdns(ch.Old)})
		}
		if err != nil {
			return fmt.Errorf("new record was created but removing the old one failed: %w", err)
		}
		return nil
	}
	return fmt.Errorf("unknown change kind %q", ch.Kind)
}
