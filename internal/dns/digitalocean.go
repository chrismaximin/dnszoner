package dns

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/digitalocean/godo"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// DigitalOcean uses the official godo client. Records have IDs, so changes
// are applied one record at a time.
type DigitalOcean struct {
	src     config.Source
	baseURL string // for tests

	mu     sync.Mutex
	client *godo.Client
}

func NewDigitalOcean(src config.Source) *DigitalOcean { return &DigitalOcean{src: src} }

func (p *DigitalOcean) api() *godo.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client == nil {
		p.client = godo.NewFromToken(p.src.APIToken)
		if p.baseURL != "" {
			p.client, _ = godo.New(p.client.HTTPClient, godo.SetBaseURL(p.baseURL))
		}
	}
	return p.client
}

// pages calls list for each page until the last one.
func pages(list func(*godo.ListOptions) (*godo.Response, error)) error {
	opt := &godo.ListOptions{PerPage: 200}
	for {
		resp, err := list(opt)
		if err != nil {
			return err
		}
		if resp == nil || resp.Links == nil || resp.Links.IsLastPage() {
			return nil
		}
		page, err := resp.Links.CurrentPage()
		if err != nil {
			return err
		}
		opt.Page = page + 1
	}
}

func (p *DigitalOcean) ListZones(ctx context.Context) ([]store.Zone, error) {
	var zones []store.Zone
	err := pages(func(opt *godo.ListOptions) (*godo.Response, error) {
		ds, resp, err := p.api().Domains.List(ctx, opt)
		for _, d := range ds {
			zones = append(zones, store.Zone{Name: strings.ToLower(d.Name), Ref: d.Name})
		}
		return resp, err
	})
	return zones, err
}

func (p *DigitalOcean) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	recs, err := doIDs{p, zone}.List(ctx)
	out := make([]store.RecordData, len(recs))
	for i, r := range recs {
		out[i] = r.Data
	}
	return out, err
}

func (p *DigitalOcean) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applyByID(ctx, doIDs{p, zone}, chs)
}

// DigitalOcean's default TTL is 30 minutes.
func (p *DigitalOcean) AutoTTL() int64           { return 1800 }
func (p *DigitalOcean) IsAutoTTL(ttl int64) bool { return false }
func (p *DigitalOcean) Concurrency() int         { return 4 }

type doIDs struct {
	p    *DigitalOcean
	zone store.Zone
}

func doToRecord(r godo.DomainRecord) store.RecordData {
	d := store.RecordData{Type: r.Type, Host: r.Name, Value: r.Data, TTL: int64(r.TTL)}
	switch r.Type {
	case "MX":
		d.Value = fmt.Sprintf("%d %s", r.Priority, r.Data)
	case "SRV":
		d.Value = fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, r.Data)
	case "CAA":
		d.Value = fmt.Sprintf("%d %s %q", r.Flags, r.Tag, r.Data)
	}
	return d
}

func doFromRecord(d store.RecordData) (*godo.DomainRecordEditRequest, error) {
	r := &godo.DomainRecordEditRequest{Type: d.Type, Name: d.Host, Data: d.Value, TTL: int(d.TTL)}
	if r.TTL == 0 {
		r.TTL = 1800
	}
	f := strings.Fields(d.Value)
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch d.Type {
	case "MX":
		if len(f) != 2 {
			return nil, fmt.Errorf("invalid MX value %q", d.Value)
		}
		r.Priority, r.Data = atoi(f[0]), f[1]
	case "SRV":
		if len(f) != 4 {
			return nil, fmt.Errorf("invalid SRV value %q", d.Value)
		}
		r.Priority, r.Weight, r.Port, r.Data = atoi(f[0]), atoi(f[1]), atoi(f[2]), f[3]
	case "CAA":
		if len(f) < 3 {
			return nil, fmt.Errorf("invalid CAA value %q", d.Value)
		}
		value := strings.Join(f[2:], " ")
		if u, err := strconv.Unquote(value); err == nil {
			value = u
		}
		r.Flags, r.Tag, r.Data = atoi(f[0]), f[1], value
	}
	return r, nil
}

func (z doIDs) List(ctx context.Context) ([]idRecord, error) {
	var out []idRecord
	err := pages(func(opt *godo.ListOptions) (*godo.Response, error) {
		recs, resp, err := z.p.api().Domains.Records(ctx, z.zone.Ref, opt)
		for _, r := range recs {
			out = append(out, idRecord{ID: strconv.Itoa(r.ID), Data: doToRecord(r)})
		}
		return resp, err
	})
	return out, err
}

func (z doIDs) Create(ctx context.Context, d store.RecordData) error {
	req, err := doFromRecord(d)
	if err == nil {
		_, _, err = z.p.api().Domains.CreateRecord(ctx, z.zone.Ref, req)
	}
	return err
}

func (z doIDs) Update(ctx context.Context, id string, d store.RecordData) error {
	n, _ := strconv.Atoi(id)
	req, err := doFromRecord(d)
	if err == nil {
		_, _, err = z.p.api().Domains.EditRecord(ctx, z.zone.Ref, n, req)
	}
	return err
}

func (z doIDs) Delete(ctx context.Context, id string) error {
	n, _ := strconv.Atoi(id)
	_, err := z.p.api().Domains.DeleteRecord(ctx, z.zone.Ref, n)
	return err
}
