package dns

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/libdns/libdns"

	"github.com/chrismaximin/dnszoner/internal/store"
)

// memAPI is an in-memory zone with libdns set semantics.
type memAPI struct {
	recs        []store.RecordData
	sets, dels  int
	failDeletes bool
	failSets    bool
}

func (m *memAPI) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	var out []libdns.Record
	for _, r := range m.recs {
		out = append(out, toLibdns(r))
	}
	return out, nil
}

func (m *memAPI) SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	m.sets++
	if m.failSets {
		return nil, errors.New("boom")
	}
	in := fromLibdns(recs)
	// Like Route53, refuse a CNAME sharing its name with other records.
	for _, n := range in {
		for _, r := range m.recs {
			if r.Host == n.Host && r.Type != n.Type && (r.Type == "CNAME" || n.Type == "CNAME") {
				return nil, fmt.Errorf("%s %s conflicts with other records with the same name", n.Type, n.Host)
			}
		}
	}
	m.recs = slices.DeleteFunc(m.recs, func(r store.RecordData) bool {
		return slices.ContainsFunc(in, func(n store.RecordData) bool { return sameSet(r, n) })
	})
	m.recs = append(m.recs, in...)
	return recs, nil
}

func (m *memAPI) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	m.dels++
	if m.failDeletes {
		return nil, errors.New("boom")
	}
	in := fromLibdns(recs)
	m.recs = slices.DeleteFunc(m.recs, func(r store.RecordData) bool {
		return slices.ContainsFunc(in, func(n store.RecordData) bool { return sameSet(r, n) && r.Value == n.Value })
	})
	return recs, nil
}

func rec(typ, host, value string, ttl int64) store.RecordData {
	return store.RecordData{Type: typ, Host: host, Value: value, TTL: ttl}
}

func (m *memAPI) dump() string {
	var lines []string
	for _, r := range m.recs {
		lines = append(lines, strings.Join([]string{r.Type, r.Host, r.Value, (time.Duration(r.TTL) * time.Second).String()}, " "))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

func TestApplyLibdnsBatch(t *testing.T) {
	api := &memAPI{recs: []store.RecordData{
		rec("A", "@", "1.1.1.1", 300),
		rec("A", "@", "2.2.2.2", 300),
		rec("A", "www", "1.1.1.1", 60),
		rec("CNAME", "old", "www.example.com.", 300),
		rec("TXT", "@", "v=spf1 ip4:1.1.1.1 ~all", 300),
	}}
	chs := []Change{
		{Kind: store.PendingUpdate, Old: rec("A", "@", "1.1.1.1", 300), New: rec("A", "@", "9.9.9.9", 600)},
		{Kind: store.PendingUpdate, Old: rec("A", "www", "1.1.1.1", 60), New: rec("A", "www", "9.9.9.9", 60)},
		{Kind: store.PendingUpdate, Old: rec("CNAME", "old", "www.example.com.", 300), New: rec("CNAME", "new", "www.example.com.", 300)},
		{Kind: store.PendingDelete, Old: rec("TXT", "@", "v=spf1 ip4:1.1.1.1 ~all", 300)},
		{Kind: store.PendingUpdate, Old: rec("A", "gone", "1.1.1.1", 300), New: rec("A", "gone", "9.9.9.9", 300)},
	}
	errs := applyLibdns(context.Background(), api, "example.com", chs)
	for i, err := range errs[:4] {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	if !errors.Is(errs[4], ErrConflict) {
		t.Errorf("missing record: got %v, want ErrConflict", errs[4])
	}
	if api.sets != 1 || api.dels != 1 {
		t.Errorf("got %d set and %d delete calls, want 1 and 1", api.sets, api.dels)
	}
	want := strings.Join([]string{
		"A @ 2.2.2.2 10m0s", // the set takes the edited record's TTL
		"A @ 9.9.9.9 10m0s",
		"A www 9.9.9.9 1m0s",
		"CNAME new www.example.com. 5m0s",
	}, "\n")
	if got := api.dump(); got != want {
		t.Errorf("zone after apply:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyLibdnsCreate(t *testing.T) {
	api := &memAPI{recs: []store.RecordData{rec("A", "@", "1.1.1.1", 300)}}
	errs := applyLibdns(context.Background(), api, "example.com", []Change{
		{Kind: store.PendingCreate, Old: rec("A", "@", "2.2.2.2", 60), New: rec("A", "@", "2.2.2.2", 60)},
		{Kind: store.PendingCreate, Old: rec("TXT", "_x", "hello", 300), New: rec("TXT", "_x", "hello", 300)},
		{Kind: store.PendingCreate, Old: rec("A", "@", "1.1.1.1", 300), New: rec("A", "@", "1.1.1.1", 300)},
	})
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("creates failed: %v", errs)
	}
	if !errors.Is(errs[2], ErrExists) {
		t.Errorf("duplicate create: got %v, want ErrExists", errs[2])
	}
	want := strings.Join([]string{"A @ 1.1.1.1 1m0s", "A @ 2.2.2.2 1m0s", "TXT _x hello 5m0s"}, "\n")
	if got := api.dump(); got != want {
		t.Errorf("zone after create:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyLibdnsDeleteFailure(t *testing.T) {
	api := &memAPI{failDeletes: true, recs: []store.RecordData{
		rec("A", "a", "1.1.1.1", 300),
		rec("A", "b", "1.1.1.1", 300),
	}}
	errs := applyLibdns(context.Background(), api, "example.com", []Change{
		{Kind: store.PendingUpdate, Old: rec("A", "a", "1.1.1.1", 300), New: rec("A", "a2", "1.1.1.1", 300)},
		{Kind: store.PendingUpdate, Old: rec("A", "b", "1.1.1.1", 300), New: rec("A", "b", "2.2.2.2", 300)},
	})
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "removing the old one failed") {
		t.Errorf("renamed record: got %v", errs[0])
	}
	if errs[1] != nil {
		t.Errorf("in-place update should not be affected by the delete failure: %v", errs[1])
	}
}

func TestApplyLibdnsCNAMESwap(t *testing.T) {
	api := &memAPI{recs: []store.RecordData{
		rec("A", "www", "1.1.1.1", 300),
		rec("CNAME", "app", "www.example.com.", 300),
		rec("A", "a", "1.1.1.1", 300),
	}}
	errs := applyLibdns(context.Background(), api, "example.com", []Change{
		{Kind: store.PendingUpdate, Old: rec("A", "www", "1.1.1.1", 300), New: rec("CNAME", "www", "host.example.net.", 300)},
		{Kind: store.PendingUpdate, Old: rec("CNAME", "app", "www.example.com.", 300), New: rec("A", "app", "2.2.2.2", 300)},
		{Kind: store.PendingUpdate, Old: rec("A", "a", "1.1.1.1", 300), New: rec("A", "a2", "1.1.1.1", 300)},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	want := strings.Join([]string{"A a2 1.1.1.1 5m0s", "A app 2.2.2.2 5m0s", "CNAME www host.example.net. 5m0s"}, "\n")
	if got := api.dump(); got != want {
		t.Errorf("zone after apply:\n%s\nwant:\n%s", got, want)
	}
	// The clashing sets go first; the renamed one after the write.
	if api.dels != 2 || api.sets != 1 {
		t.Errorf("got %d set and %d delete calls, want 1 and 2", api.sets, api.dels)
	}

	api = &memAPI{failSets: true, recs: []store.RecordData{rec("A", "www", "1.1.1.1", 300)}}
	errs = applyLibdns(context.Background(), api, "example.com", []Change{
		{Kind: store.PendingUpdate, Old: rec("A", "www", "1.1.1.1", 300), New: rec("CNAME", "www", "host.example.net.", 300)},
	})
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "old record was removed") {
		t.Errorf("failed write after delete: got %v", errs[0])
	}
}
