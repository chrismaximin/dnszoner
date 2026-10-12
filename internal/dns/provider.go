// Package dns talks to DNS providers: listing zones, fetching records and
// applying record changes.
package dns

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/libdns/libdns"

	"github.com/chrismaximin/dnszoner/internal/config"
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
	// Apply applies changes to one zone, returning one error per change.
	Apply(ctx context.Context, zone store.Zone, chs []Change) []error
	// AutoTTL is the TTL used when the user types "auto".
	AutoTTL() int64
	// IsAutoTTL reports whether a TTL is the provider's automatic value.
	IsAutoTTL(ttl int64) bool
	// Concurrency is how many zones may be fetched at once during a sync.
	Concurrency() int
}

// New returns the provider for a configured source.
func New(src config.Source) Provider {
	if src.Provider == "namecheap" {
		return NewNamecheap(src)
	}
	return NewRoute53(src)
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

// applyLibdns applies a zone's changes with libdns semantics, where records
// sharing a (host, type) form one set with one TTL. It re-reads the zone once,
// computes the final content of every set the changes touch, and writes them
// in one SetRecords call (one API change for Route53 and Namecheap), then
// deletes the sets left empty. It returns one error per change: a change whose
// old record is gone or changed fails with ErrConflict and is left out.
func applyLibdns(ctx context.Context, api recordAPI, zone string, chs []Change) []error {
	errs := make([]error, len(chs))
	failAll := func(err error) []error {
		for i := range errs {
			if errs[i] == nil {
				errs[i] = err
			}
		}
		return errs
	}

	recs, err := api.GetRecords(ctx, fqdn(zone))
	if err != nil {
		return failAll(fmt.Errorf("fetching current records: %w", err))
	}
	type setKey struct{ host, typ string }
	keyOf := func(d store.RecordData) setKey { return setKey{d.Host, strings.ToUpper(d.Type)} }
	orig := map[setKey][]store.RecordData{}
	for _, r := range fromLibdns(recs) {
		orig[keyOf(r)] = append(orig[keyOf(r)], r)
	}

	sets := map[setKey][]store.RecordData{}
	get := func(k setKey) []store.RecordData {
		if s, ok := sets[k]; ok {
			return s
		}
		return slices.Clone(orig[k])
	}
	var touched []setKey
	put := func(k setKey, s []store.RecordData) {
		if _, ok := sets[k]; !ok {
			touched = append(touched, k)
		}
		sets[k] = s
	}
	without := func(s []store.RecordData, value string) []store.RecordData {
		return slices.DeleteFunc(s, func(r store.RecordData) bool { return r.Value == value })
	}

	for i, ch := range chs {
		k := keyOf(ch.Old)
		set := get(k)
		if !slices.ContainsFunc(set, func(r store.RecordData) bool { return r.Value == ch.Old.Value }) {
			errs[i] = ErrConflict
			continue
		}
		put(k, without(set, ch.Old.Value)) // also drops exact duplicates
		switch ch.Kind {
		case store.PendingDelete:
		case store.PendingUpdate:
			nk := keyOf(ch.New)
			ns := append(without(get(nk), ch.New.Value), ch.New)
			for j := range ns {
				ns[j].TTL = ch.New.TTL
			}
			put(nk, ns)
		default:
			errs[i] = fmt.Errorf("unknown change kind %q", ch.Kind)
		}
	}

	var set, del []libdns.Record
	emptied := map[setKey]bool{}
	for _, k := range touched {
		if len(sets[k]) == 0 {
			emptied[k] = true
			for _, r := range orig[k] {
				del = append(del, toLibdns(r))
			}
			continue
		}
		for _, r := range sets[k] {
			set = append(set, toLibdns(r))
		}
	}
	// Writing before deleting means a renamed record exists under its new
	// name before the old one goes away.
	if len(set) > 0 {
		if _, err := api.SetRecords(ctx, fqdn(zone), set); err != nil {
			return failAll(err)
		}
	}
	if len(del) > 0 {
		if _, err := api.DeleteRecords(ctx, fqdn(zone), del); err != nil {
			for i, ch := range chs {
				if errs[i] != nil || !emptied[keyOf(ch.Old)] {
					continue
				}
				if ch.Kind == store.PendingUpdate {
					errs[i] = fmt.Errorf("new record was created but removing the old one failed: %w", err)
				} else {
					errs[i] = err
				}
			}
		}
	}
	return errs
}
