package dns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// Azure uses Azure DNS (public zones) in one subscription. Record sets carry
// an ETag, so a set changed elsewhere since it was read is refused rather
// than overwritten.
type Azure struct {
	src config.Source

	// For tests.
	cred azcore.TokenCredential
	opts *arm.ClientOptions

	mu      sync.Mutex
	factory *armdns.ClientFactory
}

func NewAzure(src config.Source) *Azure { return &Azure{src: src} }

func (p *Azure) api() (*armdns.ClientFactory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.factory != nil {
		return p.factory, nil
	}
	cred := p.cred
	if cred == nil {
		var err error
		if p.src.ClientID != "" {
			cred, err = azidentity.NewClientSecretCredential(p.src.TenantID, p.src.ClientID, p.src.ClientSecret, nil)
		} else {
			cred, err = azidentity.NewDefaultAzureCredential(nil)
		}
		if err != nil {
			return nil, fmt.Errorf("azure credentials (set a service principal, or run az login): %w", err)
		}
	}
	f, err := armdns.NewClientFactory(p.src.SubscriptionID, cred, p.opts)
	if err != nil {
		return nil, err
	}
	p.factory = f
	return f, nil
}

// resourceGroup reads the resource group out of a resource ID.
func resourceGroup(id string) string {
	parts := strings.Split(id, "/")
	for i, part := range parts {
		if strings.EqualFold(part, "resourceGroups") && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func (p *Azure) ListZones(ctx context.Context) ([]store.Zone, error) {
	f, err := p.api()
	if err != nil {
		return nil, err
	}
	var zones []store.Zone
	pager := f.NewZonesClient().NewListPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, z := range page.Value {
			name := strings.ToLower(str(z.Name))
			zones = append(zones, store.Zone{Name: name, Ref: resourceGroup(str(z.ID)) + "/" + name})
		}
	}
	return zones, nil
}

func (p *Azure) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	return azSets{p, zone}.Get(ctx)
}

func (p *Azure) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applySets(ctx, azSets{p, zone}, chs)
}

// Azure DNS always needs a TTL; the portal's default is an hour.
func (p *Azure) AutoTTL() int64           { return 3600 }
func (p *Azure) IsAutoTTL(ttl int64) bool { return false }
func (p *Azure) Concurrency() int         { return 4 }

type azSets struct {
	p    *Azure
	zone store.Zone
}

func (s azSets) group() string {
	rg, _, _ := strings.Cut(s.zone.Ref, "/")
	return rg
}

// azValues lists the values of a record set in dnszoner's form.
func azValues(typ string, p *armdns.RecordSetProperties) []string {
	var out []string
	i32 := func(n *int32) int32 { return val(n) }
	switch typ {
	case "A":
		for _, r := range p.ARecords {
			out = append(out, str(r.IPv4Address))
		}
	case "AAAA":
		for _, r := range p.AaaaRecords {
			out = append(out, str(r.IPv6Address))
		}
	case "CNAME":
		if p.CnameRecord != nil {
			out = append(out, str(p.CnameRecord.Cname))
		}
	case "MX":
		for _, r := range p.MxRecords {
			out = append(out, fmt.Sprintf("%d %s", i32(r.Preference), str(r.Exchange)))
		}
	case "NS":
		for _, r := range p.NsRecords {
			out = append(out, str(r.Nsdname))
		}
	case "PTR":
		for _, r := range p.PtrRecords {
			out = append(out, str(r.Ptrdname))
		}
	case "TXT":
		for _, r := range p.TxtRecords {
			var b strings.Builder
			for _, c := range r.Value {
				b.WriteString(str(c))
			}
			out = append(out, b.String())
		}
	case "SRV":
		for _, r := range p.SrvRecords {
			out = append(out, fmt.Sprintf("%d %d %d %s", i32(r.Priority), i32(r.Weight), i32(r.Port), str(r.Target)))
		}
	case "CAA":
		for _, r := range p.CaaRecords {
			out = append(out, fmt.Sprintf("%d %s %q", i32(r.Flags), str(r.Tag), str(r.Value)))
		}
	}
	return out
}

// azProps builds the properties of a record set from its records.
func azProps(set []store.RecordData) (*armdns.RecordSetProperties, error) {
	typ := set[0].Type
	p := &armdns.RecordSetProperties{TTL: to.Ptr(max(1, set[0].TTL))}
	i32 := func(s string) *int32 { n, _ := strconv.ParseInt(s, 10, 32); return to.Ptr(int32(n)) }
	for _, r := range set {
		f := strings.Fields(r.Value)
		switch typ {
		case "A":
			p.ARecords = append(p.ARecords, &armdns.ARecord{IPv4Address: to.Ptr(r.Value)})
		case "AAAA":
			p.AaaaRecords = append(p.AaaaRecords, &armdns.AaaaRecord{IPv6Address: to.Ptr(r.Value)})
		case "CNAME":
			if len(set) > 1 {
				return nil, errors.New("a CNAME record set can only hold one record")
			}
			p.CnameRecord = &armdns.CnameRecord{Cname: to.Ptr(r.Value)}
		case "MX":
			if len(f) != 2 {
				return nil, fmt.Errorf("invalid MX value %q", r.Value)
			}
			p.MxRecords = append(p.MxRecords, &armdns.MxRecord{Preference: i32(f[0]), Exchange: to.Ptr(f[1])})
		case "NS":
			p.NsRecords = append(p.NsRecords, &armdns.NsRecord{Nsdname: to.Ptr(r.Value)})
		case "PTR":
			p.PtrRecords = append(p.PtrRecords, &armdns.PtrRecord{Ptrdname: to.Ptr(r.Value)})
		case "TXT":
			var chunks []*string
			for _, c := range records.TXTChunks(r.Value) {
				chunks = append(chunks, to.Ptr(c))
			}
			p.TxtRecords = append(p.TxtRecords, &armdns.TxtRecord{Value: chunks})
		case "SRV":
			if len(f) != 4 {
				return nil, fmt.Errorf("invalid SRV value %q", r.Value)
			}
			p.SrvRecords = append(p.SrvRecords, &armdns.SrvRecord{Priority: i32(f[0]), Weight: i32(f[1]), Port: i32(f[2]), Target: to.Ptr(f[3])})
		case "CAA":
			if len(f) < 3 {
				return nil, fmt.Errorf("invalid CAA value %q", r.Value)
			}
			value := strings.Join(f[2:], " ")
			if u, err := strconv.Unquote(value); err == nil {
				value = u
			}
			p.CaaRecords = append(p.CaaRecords, &armdns.CaaRecord{Flags: i32(f[0]), Tag: to.Ptr(f[1]), Value: to.Ptr(value)})
		default:
			return nil, fmt.Errorf("Azure DNS doesn't support %s records", typ)
		}
	}
	return p, nil
}

func (s azSets) Get(ctx context.Context) ([]store.RecordData, error) {
	f, err := s.p.api()
	if err != nil {
		return nil, err
	}
	var out []store.RecordData
	pager := f.NewRecordSetsClient().NewListAllByDNSZonePager(s.group(), s.zone.Name, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, set := range page.Value {
			typ := str(set.Type)
			typ = typ[strings.LastIndex(typ, "/")+1:]
			p := set.Properties
			if p == nil || typ == "SOA" || p.TargetResource != nil && p.TargetResource.ID != nil {
				continue // the SOA, and alias sets pointing at Azure resources
			}
			for _, v := range azValues(typ, p) {
				out = append(out, store.RecordData{Type: typ, Host: str(set.Name), Value: v, TTL: val(p.TTL)})
			}
		}
	}
	return out, nil
}

// current returns the set as Azure has it, or nil.
func (s azSets) current(ctx context.Context, rs *armdns.RecordSetsClient, host, typ string) (*armdns.RecordSet, error) {
	res, err := rs.Get(ctx, s.group(), s.zone.Name, host, armdns.RecordType(typ), nil)
	var rerr *azcore.ResponseError
	if errors.As(err, &rerr) && rerr.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p := res.Properties; p != nil && p.TargetResource != nil && p.TargetResource.ID != nil {
		return nil, fmt.Errorf("%s %s is an alias of an Azure resource, which dnszoner can't edit", typ, host)
	}
	return &res.RecordSet, nil
}

func (s azSets) Set(ctx context.Context, recs []store.RecordData) error {
	f, err := s.p.api()
	if err != nil {
		return err
	}
	rs := f.NewRecordSetsClient()
	for _, set := range group(recs) {
		typ, host := set[0].Type, set[0].Host
		props, err := azProps(set)
		if err != nil {
			return err
		}
		cur, err := s.current(ctx, rs, host, typ)
		if err != nil {
			return err
		}
		opts := &armdns.RecordSetsClientCreateOrUpdateOptions{IfNoneMatch: to.Ptr("*")}
		if cur != nil {
			opts = &armdns.RecordSetsClientCreateOrUpdateOptions{IfMatch: cur.Etag}
			if cur.Properties != nil {
				props.Metadata = cur.Properties.Metadata
			}
		}
		if _, err := rs.CreateOrUpdate(ctx, s.group(), s.zone.Name, host, armdns.RecordType(typ), armdns.RecordSet{Properties: props}, opts); err != nil {
			return err
		}
	}
	return nil
}

func (s azSets) Delete(ctx context.Context, recs []store.RecordData) error {
	f, err := s.p.api()
	if err != nil {
		return err
	}
	rs := f.NewRecordSetsClient()
	for _, set := range group(recs) {
		cur, err := s.current(ctx, rs, set[0].Host, set[0].Type)
		if err != nil {
			return err
		}
		if cur == nil {
			continue
		}
		if _, err := rs.Delete(ctx, s.group(), s.zone.Name, set[0].Host, armdns.RecordType(set[0].Type), &armdns.RecordSetsClientDeleteOptions{IfMatch: cur.Etag}); err != nil {
			return err
		}
	}
	return nil
}

func str(p *string) string { return val(p) }

// val dereferences p, or returns the zero value for nil.
func val[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
