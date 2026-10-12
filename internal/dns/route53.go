package dns

import (
	"context"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	r53 "github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/libdns/route53"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

type Route53 struct {
	src config.Source

	mu        sync.Mutex
	client    *r53.Client
	providers map[string]*route53.Provider // by hosted zone ID
}

func NewRoute53(src config.Source) *Route53 {
	return &Route53{src: src, providers: map[string]*route53.Provider{}}
}

func (p *Route53) awsClient(ctx context.Context) (*r53.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.client != nil {
		return p.client, nil
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if p.src.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(p.src.Profile))
	}
	region := p.src.Region
	if region == "" {
		region = "us-east-1" // Route53 is global
	}
	opts = append(opts, awsconfig.WithRegion(region))
	if p.src.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(p.src.AccessKeyID, p.src.SecretAccessKey, p.src.SessionToken)))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	p.client = r53.NewFromConfig(cfg)
	return p.client, nil
}

func (p *Route53) ListZones(ctx context.Context) ([]store.Zone, error) {
	client, err := p.awsClient(ctx)
	if err != nil {
		return nil, err
	}
	var zones []store.Zone
	pager := r53.NewListHostedZonesPaginator(client, &r53.ListHostedZonesInput{})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, hz := range page.HostedZones {
			z := store.Zone{
				Name: strings.TrimSuffix(aws.ToString(hz.Name), "."),
				Ref:  strings.TrimPrefix(aws.ToString(hz.Id), "/hostedzone/"),
			}
			if hz.Config != nil && hz.Config.PrivateZone {
				z.Note = "private"
			}
			zones = append(zones, z)
		}
	}
	return zones, nil
}

// zoneProvider returns a libdns provider pinned to one hosted zone, so that
// public and private zones with the same name are never confused.
func (p *Route53) zoneProvider(zone store.Zone) *route53.Provider {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prov, ok := p.providers[zone.Ref]; ok {
		return prov
	}
	prov := &route53.Provider{
		Region:          p.src.Region,
		Profile:         p.src.Profile,
		AccessKeyId:     p.src.AccessKeyID,
		SecretAccessKey: p.src.SecretAccessKey,
		SessionToken:    p.src.SessionToken,
		HostedZoneID:    zone.Ref,
	}
	if prov.Region == "" {
		prov.Region = "us-east-1"
	}
	p.providers[zone.Ref] = prov
	return prov
}

func (p *Route53) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	recs, err := p.zoneProvider(zone).GetRecords(ctx, fqdn(zone.Name))
	if err != nil {
		return nil, err
	}
	return fromLibdns(recs), nil
}

func (p *Route53) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applyLibdns(ctx, p.zoneProvider(zone), zone.Name, chs)
}

func (p *Route53) AutoTTL() int64           { return 300 }
func (p *Route53) IsAutoTTL(ttl int64) bool { return false }
func (p *Route53) Concurrency() int         { return 4 }
