package dns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gdns "google.golang.org/api/dns/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// GoogleCloud uses Cloud DNS in one GCP project. Records are RRSets,
// changed through atomic changes that must name the exact sets they
// replace, so a set changed elsewhere since it was read is refused.
type GoogleCloud struct {
	src      config.Source
	endpoint string // for tests

	mu  sync.Mutex
	svc *gdns.Service
}

func NewGoogleCloud(src config.Source) *GoogleCloud { return &GoogleCloud{src: src} }

func (p *GoogleCloud) api(ctx context.Context) (*gdns.Service, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.svc != nil {
		return p.svc, nil
	}
	opts := []option.ClientOption{option.WithScopes(gdns.NdevClouddnsReadwriteScope)}
	switch {
	case p.endpoint != "":
		opts = append(opts, option.WithEndpoint(p.endpoint), option.WithoutAuthentication())
	case p.src.CredentialsFile != "":
		path := p.src.CredentialsFile
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		opts = append(opts, option.WithCredentialsFile(path))
	}
	svc, err := gdns.NewService(context.WithoutCancel(ctx), opts...)
	if err != nil {
		return nil, fmt.Errorf("google cloud credentials (set credentials_file, or run gcloud auth application-default login): %w", err)
	}
	p.svc = svc
	return svc, nil
}

func (p *GoogleCloud) ListZones(ctx context.Context) ([]store.Zone, error) {
	svc, err := p.api(ctx)
	if err != nil {
		return nil, err
	}
	var zones []store.Zone
	err = svc.ManagedZones.List(p.src.Project).Pages(ctx, func(r *gdns.ManagedZonesListResponse) error {
		for _, mz := range r.ManagedZones {
			z := store.Zone{Name: strings.ToLower(strings.TrimSuffix(mz.DnsName, ".")), Ref: mz.Name}
			if mz.Visibility == "private" {
				z.Note = "private"
			}
			zones = append(zones, z)
		}
		return nil
	})
	return zones, err
}

func (p *GoogleCloud) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	return gcSets{p, zone}.Get(ctx)
}

func (p *GoogleCloud) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applySets(ctx, gcSets{p, zone}, chs)
}

// Cloud DNS always needs a TTL; 300 seconds is its usual default.
func (p *GoogleCloud) AutoTTL() int64           { return 300 }
func (p *GoogleCloud) IsAutoTTL(ttl int64) bool { return false }
func (p *GoogleCloud) Concurrency() int         { return 4 }

type gcSets struct {
	p    *GoogleCloud
	zone store.Zone
}

func (s gcSets) name(host string) string {
	if host == "@" || host == "" {
		return s.zone.Name + "."
	}
	return host + "." + s.zone.Name + "."
}

func isTXT(typ string) bool { return typ == "TXT" || typ == "SPF" }

func (s gcSets) Get(ctx context.Context) ([]store.RecordData, error) {
	svc, err := s.p.api(ctx)
	if err != nil {
		return nil, err
	}
	var out []store.RecordData
	err = svc.ResourceRecordSets.List(s.p.src.Project, s.zone.Ref).Pages(ctx, func(r *gdns.ResourceRecordSetsListResponse) error {
		for _, set := range r.Rrsets {
			if set.RoutingPolicy != nil {
				continue // weighted or geo sets have no plain values to edit
			}
			host := records.NormalizeHost(set.Name, s.zone.Name)
			for _, v := range set.Rrdatas {
				if isTXT(set.Type) {
					v = records.UnquoteTXT(v)
				}
				out = append(out, store.RecordData{Type: set.Type, Host: host, Value: v, TTL: set.Ttl})
			}
		}
		return nil
	})
	return out, err
}

// current returns the set as Cloud DNS has it, or nil.
func (s gcSets) current(ctx context.Context, svc *gdns.Service, host, typ string) (*gdns.ResourceRecordSet, error) {
	set, err := svc.ResourceRecordSets.Get(s.p.src.Project, s.zone.Ref, s.name(host), typ).Context(ctx).Do()
	var gerr *googleapi.Error
	if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
		return nil, nil
	}
	if err == nil && set.RoutingPolicy != nil {
		return nil, fmt.Errorf("%s %s uses a routing policy, which dnszoner can't edit", typ, s.name(host))
	}
	return set, err
}

func (s gcSets) Set(ctx context.Context, recs []store.RecordData) error {
	svc, err := s.p.api(ctx)
	if err != nil {
		return err
	}
	ch := &gdns.Change{}
	for _, set := range group(recs) {
		typ, host := set[0].Type, set[0].Host
		cur, err := s.current(ctx, svc, host, typ)
		if err != nil {
			return err
		}
		if cur != nil {
			ch.Deletions = append(ch.Deletions, cur)
		}
		add := &gdns.ResourceRecordSet{Name: s.name(host), Type: typ, Ttl: set[0].TTL}
		if add.Ttl <= 0 {
			add.Ttl = s.p.AutoTTL()
		}
		for _, r := range set {
			v := r.Value
			if isTXT(typ) {
				v = records.QuoteTXT(v)
			}
			add.Rrdatas = append(add.Rrdatas, v)
		}
		ch.Additions = append(ch.Additions, add)
	}
	return s.commit(ctx, svc, ch)
}

func (s gcSets) Delete(ctx context.Context, recs []store.RecordData) error {
	svc, err := s.p.api(ctx)
	if err != nil {
		return err
	}
	ch := &gdns.Change{}
	for _, set := range group(recs) {
		cur, err := s.current(ctx, svc, set[0].Host, set[0].Type)
		if err != nil {
			return err
		}
		if cur != nil {
			ch.Deletions = append(ch.Deletions, cur)
		}
	}
	if len(ch.Deletions) == 0 {
		return nil
	}
	return s.commit(ctx, svc, ch)
}

// commit sends the change and waits until Cloud DNS has applied it.
func (s gcSets) commit(ctx context.Context, svc *gdns.Service, ch *gdns.Change) error {
	c, err := svc.Changes.Create(s.p.src.Project, s.zone.Ref, ch).Context(ctx).Do()
	for err == nil && c.Status != "done" {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		c, err = svc.Changes.Get(s.p.src.Project, s.zone.Ref, c.Id).Context(ctx).Do()
	}
	return err
}
