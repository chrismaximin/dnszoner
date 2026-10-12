package dns

import (
	"context"
	"strconv"
	"sync"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// Hetzner uses the DNS API of the Hetzner Cloud API (Hetzner Console),
// which replaced the old DNS Console API in 2026. Its records come in
// RRSets (one per host and type), changed through asynchronous actions.
type Hetzner struct {
	src      config.Source
	endpoint string // for tests

	mu     sync.Mutex
	client *hcloud.Client
}

func NewHetzner(src config.Source) *Hetzner { return &Hetzner{src: src} }

func (h *Hetzner) api() *hcloud.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client == nil {
		opts := []hcloud.ClientOption{hcloud.WithToken(h.src.APIToken), hcloud.WithApplication("dnszoner", "")}
		if h.endpoint != "" {
			opts = append(opts, hcloud.WithEndpoint(h.endpoint))
		}
		h.client = hcloud.NewClient(opts...)
	}
	return h.client
}

func (h *Hetzner) ListZones(ctx context.Context) ([]store.Zone, error) {
	zs, err := h.api().Zone.All(ctx)
	if err != nil {
		return nil, err
	}
	zones := make([]store.Zone, len(zs))
	for i, z := range zs {
		zones[i] = store.Zone{Name: z.Name, Ref: strconv.FormatInt(z.ID, 10)}
		if z.Mode == hcloud.ZoneModeSecondary {
			zones[i].Note = "secondary"
		}
	}
	return zones, nil
}

func (h *Hetzner) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	return hetznerSets{h, zone}.Get(ctx)
}

func (h *Hetzner) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applySets(ctx, hetznerSets{h, zone}, chs)
}

// Hetzner RRSets without a TTL use the zone's default.
func (h *Hetzner) AutoTTL() int64           { return 0 }
func (h *Hetzner) IsAutoTTL(ttl int64) bool { return ttl == 0 }
func (h *Hetzner) Concurrency() int         { return 3 }

type hetznerSets struct {
	h    *Hetzner
	zone store.Zone
}

func (s hetznerSets) hz() *hcloud.Zone {
	id, _ := strconv.ParseInt(s.zone.Ref, 10, 64)
	return &hcloud.Zone{ID: id, Name: s.zone.Name}
}

func (s hetznerSets) Get(ctx context.Context) ([]store.RecordData, error) {
	sets, err := s.h.api().Zone.AllRRSets(ctx, s.hz())
	if err != nil {
		return nil, err
	}
	var out []store.RecordData
	for _, set := range sets {
		var ttl int64
		if set.TTL != nil {
			ttl = int64(*set.TTL)
		}
		for _, r := range set.Records {
			v := r.Value
			if set.Type == hcloud.ZoneRRSetTypeTXT {
				v = records.UnquoteTXT(v) // stored in zone file form
			}
			out = append(out, store.RecordData{Type: string(set.Type), Host: set.Name, Value: v, TTL: ttl})
		}
	}
	return out, nil
}

func (s hetznerSets) wait(ctx context.Context, a *hcloud.Action) error {
	if a == nil {
		return nil
	}
	return s.h.api().Action.WaitFor(ctx, a)
}

func (s hetznerSets) Set(ctx context.Context, recs []store.RecordData) error {
	api := s.h.api()
	for _, set := range group(recs) {
		typ, host := hcloud.ZoneRRSetType(set[0].Type), set[0].Host
		var ttl *int
		if t := int(set[0].TTL); t > 0 {
			ttl = &t
		}
		values := make([]hcloud.ZoneRRSetRecord, len(set))
		for i, r := range set {
			values[i].Value = r.Value
			if typ == hcloud.ZoneRRSetTypeTXT {
				values[i].Value = records.QuoteTXT(r.Value)
			}
		}

		existing, _, err := api.Zone.GetRRSetByNameAndType(ctx, s.hz(), host, typ)
		if err != nil {
			return err
		}
		if existing == nil {
			res, _, err := api.Zone.CreateRRSet(ctx, s.hz(), hcloud.ZoneRRSetCreateOpts{Name: host, Type: typ, TTL: ttl, Records: values})
			if err != nil {
				return err
			}
			if err := s.wait(ctx, res.Action); err != nil {
				return err
			}
			continue
		}
		existing.Zone = s.hz()
		a, _, err := api.Zone.SetRRSetRecords(ctx, existing, hcloud.ZoneRRSetSetRecordsOpts{Records: values})
		if err != nil {
			return err
		}
		if err := s.wait(ctx, a); err != nil {
			return err
		}
		if !sameTTL(existing.TTL, ttl) {
			a, _, err := api.Zone.ChangeRRSetTTL(ctx, existing, hcloud.ZoneRRSetChangeTTLOpts{TTL: ttl})
			if err != nil {
				return err
			}
			if err := s.wait(ctx, a); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameTTL(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func (s hetznerSets) Delete(ctx context.Context, recs []store.RecordData) error {
	api := s.h.api()
	for _, set := range group(recs) {
		existing, _, err := api.Zone.GetRRSetByNameAndType(ctx, s.hz(), set[0].Host, hcloud.ZoneRRSetType(set[0].Type))
		if err != nil {
			return err
		}
		if existing == nil {
			continue // already gone
		}
		existing.Zone = s.hz()
		res, _, err := api.Zone.DeleteRRSet(ctx, existing)
		if err != nil {
			return err
		}
		if err := s.wait(ctx, res.Action); err != nil {
			return err
		}
	}
	return nil
}
