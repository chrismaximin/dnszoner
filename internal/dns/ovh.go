package dns

import (
	"context"
	"strings"
	"sync"

	"github.com/libdns/ovh"
	goovh "github.com/ovh/go-ovh/ovh"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// OVH uses libdns for records, which replaces whole sets and refreshes the
// zone after changes, and the OVH API to list zones.
type OVH struct {
	src config.Source

	mu       sync.Mutex
	client   *goovh.Client
	provider *ovh.Provider
}

func NewOVH(src config.Source) *OVH { return &OVH{src: src} }

func (p *OVH) endpoint() string {
	if p.src.Endpoint != "" {
		return p.src.Endpoint
	}
	return "ovh-eu"
}

func (p *OVH) libdns() *ovh.Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider == nil {
		p.provider = &ovh.Provider{
			Endpoint:          p.endpoint(),
			ApplicationKey:    p.src.ApplicationKey,
			ApplicationSecret: p.src.ApplicationSecret,
			ConsumerKey:       p.src.ConsumerKey,
		}
	}
	return p.provider
}

func (p *OVH) ListZones(ctx context.Context) ([]store.Zone, error) {
	p.mu.Lock()
	if p.client == nil {
		c, err := goovh.NewClient(p.endpoint(), p.src.ApplicationKey, p.src.ApplicationSecret, p.src.ConsumerKey)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.client = c
	}
	c := p.client
	p.mu.Unlock()

	var names []string
	if err := c.GetWithContext(ctx, "/domain/zone", &names); err != nil {
		return nil, err
	}
	zones := make([]store.Zone, len(names))
	for i, n := range names {
		n = strings.ToLower(n)
		zones[i] = store.Zone{Name: n, Ref: n}
	}
	return zones, nil
}

func (p *OVH) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	recs, err := p.libdns().GetRecords(ctx, fqdn(zone.Name))
	if err != nil {
		return nil, err
	}
	return fromLibdns(recs), nil
}

func (p *OVH) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applyLibdns(ctx, p.libdns(), zone.Name, chs)
}

// OVH uses a TTL of 0 for the zone's default.
func (p *OVH) AutoTTL() int64           { return 0 }
func (p *OVH) IsAutoTTL(ttl int64) bool { return ttl == 0 }
func (p *OVH) Concurrency() int         { return 2 }
