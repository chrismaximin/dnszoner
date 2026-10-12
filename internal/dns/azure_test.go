package dns

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	azfake "github.com/Azure/azure-sdk-for-go/sdk/azcore/fake"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/dns/armdns/fake"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// fakeAzure keeps record sets with ETags, enforcing If-Match and
// If-None-Match like Azure DNS.
type fakeAzure struct {
	sets  map[string]*armdns.RecordSet // by "name TYPE"
	etags int
}

func (f *fakeAzure) put(name, typ string, p *armdns.RecordSetProperties) {
	f.etags++
	f.sets[name+" "+typ] = &armdns.RecordSet{Name: to.Ptr(name), Type: to.Ptr("Microsoft.Network/dnszones/" + typ),
		Etag: to.Ptr(fmt.Sprint("etag", f.etags)), Properties: p}
}

func (f *fakeAzure) server() *fake.ServerFactory {
	return &fake.ServerFactory{
		ZonesServer: fake.ZonesServer{
			NewListPager: func(*armdns.ZonesClientListOptions) (resp azfake.PagerResponder[armdns.ZonesClientListResponse]) {
				resp.AddPage(http.StatusOK, armdns.ZonesClientListResponse{ZoneListResult: armdns.ZoneListResult{Value: []*armdns.Zone{
					{Name: to.Ptr("example.com"), ID: to.Ptr("/subscriptions/sub/resourceGroups/dns-rg/providers/Microsoft.Network/dnszones/example.com")}}}}, nil)
				resp.AddPage(http.StatusOK, armdns.ZonesClientListResponse{ZoneListResult: armdns.ZoneListResult{Value: []*armdns.Zone{
					{Name: to.Ptr("b.org"), ID: to.Ptr("/subscriptions/sub/resourceGroups/other/providers/Microsoft.Network/dnszones/b.org")}}}}, nil)
				return
			},
		},
		RecordSetsServer: fake.RecordSetsServer{
			NewListAllByDNSZonePager: func(rg, zone string, _ *armdns.RecordSetsClientListAllByDNSZoneOptions) (resp azfake.PagerResponder[armdns.RecordSetsClientListAllByDNSZoneResponse]) {
				keys := make([]string, 0, len(f.sets))
				for k := range f.sets {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				var sets []*armdns.RecordSet
				for _, k := range keys {
					sets = append(sets, f.sets[k])
				}
				resp.AddPage(http.StatusOK, armdns.RecordSetsClientListAllByDNSZoneResponse{RecordSetListResult: armdns.RecordSetListResult{Value: sets}}, nil)
				return
			},
			Get: func(_ context.Context, rg, zone, name string, typ armdns.RecordType, _ *armdns.RecordSetsClientGetOptions) (resp azfake.Responder[armdns.RecordSetsClientGetResponse], errResp azfake.ErrorResponder) {
				if set, ok := f.sets[name+" "+string(typ)]; ok {
					resp.SetResponse(http.StatusOK, armdns.RecordSetsClientGetResponse{RecordSet: *set}, nil)
				} else {
					errResp.SetResponseError(http.StatusNotFound, "NotFound")
				}
				return
			},
			CreateOrUpdate: func(_ context.Context, rg, zone, name string, typ armdns.RecordType, rs armdns.RecordSet, o *armdns.RecordSetsClientCreateOrUpdateOptions) (resp azfake.Responder[armdns.RecordSetsClientCreateOrUpdateResponse], errResp azfake.ErrorResponder) {
				cur, exists := f.sets[name+" "+string(typ)]
				if o.IfNoneMatch != nil && exists || o.IfMatch != nil && (!exists || *o.IfMatch != *cur.Etag) {
					errResp.SetResponseError(http.StatusPreconditionFailed, "PreconditionFailed")
					return
				}
				f.put(name, string(typ), rs.Properties)
				resp.SetResponse(http.StatusOK, armdns.RecordSetsClientCreateOrUpdateResponse{RecordSet: *f.sets[name+" "+string(typ)]}, nil)
				return
			},
			Delete: func(_ context.Context, rg, zone, name string, typ armdns.RecordType, o *armdns.RecordSetsClientDeleteOptions) (resp azfake.Responder[armdns.RecordSetsClientDeleteResponse], errResp azfake.ErrorResponder) {
				cur, exists := f.sets[name+" "+string(typ)]
				if !exists || o.IfMatch != nil && *o.IfMatch != *cur.Etag {
					errResp.SetResponseError(http.StatusPreconditionFailed, "PreconditionFailed")
					return
				}
				delete(f.sets, name+" "+string(typ))
				resp.SetResponse(http.StatusOK, armdns.RecordSetsClientDeleteResponse{}, nil)
				return
			},
		},
	}
}

func TestAzure(t *testing.T) {
	f := &fakeAzure{sets: map[string]*armdns.RecordSet{}}
	f.put("@", "A", &armdns.RecordSetProperties{TTL: to.Ptr[int64](3600), Metadata: map[string]*string{"owner": to.Ptr("ops")},
		ARecords: []*armdns.ARecord{{IPv4Address: to.Ptr("192.0.2.1")}, {IPv4Address: to.Ptr("192.0.2.2")}}})
	f.put("@", "MX", &armdns.RecordSetProperties{TTL: to.Ptr[int64](3600), MxRecords: []*armdns.MxRecord{{Preference: to.Ptr[int32](10), Exchange: to.Ptr("mx.example.com")}}})
	f.put("@", "TXT", &armdns.RecordSetProperties{TTL: to.Ptr[int64](300), TxtRecords: []*armdns.TxtRecord{{Value: []*string{to.Ptr("v=spf1 "), to.Ptr("-all")}}}})
	f.put("@", "SOA", &armdns.RecordSetProperties{TTL: to.Ptr[int64](3600), SoaRecord: &armdns.SoaRecord{}})
	f.put("app", "A", &armdns.RecordSetProperties{TTL: to.Ptr[int64](60), TargetResource: &armdns.SubResource{ID: to.Ptr("/subscriptions/sub/.../publicIPAddresses/ip")}})
	f.put("old", "CNAME", &armdns.RecordSetProperties{TTL: to.Ptr[int64](300), CnameRecord: &armdns.CnameRecord{Cname: to.Ptr("example.com")}})

	az := NewAzure(config.Source{SubscriptionID: "sub"})
	az.cred = &azfake.TokenCredential{}
	az.opts = &arm.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: fake.NewServerFactoryTransport(f.server())}}
	ctx := context.Background()

	zones, err := az.ListZones(ctx)
	if err != nil || len(zones) != 2 || zones[0].Ref != "dns-rg/example.com" || zones[1].Ref != "other/b.org" {
		t.Fatalf("ListZones (paginated) = %+v, %v", zones, err)
	}
	recs, err := az.GetRecords(ctx, zones[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Type+" "+r.Host+" "+r.Value)
	}
	if want := "A @ 192.0.2.1|A @ 192.0.2.2|MX @ 10 mx.example.com|TXT @ v=spf1 -all|CNAME old example.com"; strings.Join(got, "|") != want {
		t.Fatalf("records (SOA and alias sets left out):\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}

	long := strings.Repeat("x", 300)
	errs := az.Apply(ctx, zones[0], []Change{
		{Kind: store.PendingUpdate, Old: recs[0], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 600}},
		{Kind: store.PendingUpdate, Old: recs[3], New: store.RecordData{Type: "TXT", Host: "@", Value: long, TTL: 300}},
		{Kind: store.PendingDelete, Old: recs[4]},
		{Kind: store.PendingCreate, New: store.RecordData{Type: "SRV", Host: "_sip._tcp", Value: "1 5 5060 sip.example.com", TTL: 3600}},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	a := f.sets["@ A"].Properties
	if len(a.ARecords) != 2 || *a.ARecords[0].IPv4Address != "192.0.2.2" || *a.ARecords[1].IPv4Address != "192.0.2.9" || *a.TTL != 600 || *a.Metadata["owner"] != "ops" {
		t.Errorf("A set: %d records, TTL %d, metadata %v", len(a.ARecords), *a.TTL, a.Metadata)
	}
	if txt := f.sets["@ TXT"].Properties.TxtRecords[0].Value; len(txt) != 2 || len(*txt[0]) != 255 {
		t.Errorf("long TXT should be split at 255 bytes: %d strings", len(txt))
	}
	if _, ok := f.sets["old CNAME"]; ok {
		t.Error("CNAME should be deleted")
	}
	if srv := f.sets["_sip._tcp SRV"]; srv == nil || *srv.Properties.SrvRecords[0].Port != 5060 {
		t.Errorf("SRV set = %+v", srv)
	}
	errs = az.Apply(ctx, zones[0], []Change{{Kind: store.PendingCreate, New: store.RecordData{Type: "A", Host: "app", Value: "192.0.2.5", TTL: 60}}})
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "alias") {
		t.Errorf("alias set: %v", errs[0])
	}
}
