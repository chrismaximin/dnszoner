package dns

import (
	"context"
	"errors"
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
	in := fromLibdns(recs)
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
