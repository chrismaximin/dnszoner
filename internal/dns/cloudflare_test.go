package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// fakeCloudflare serves just enough of the Cloudflare API, paginating
// small pages to exercise the paging.
type fakeCloudflare struct {
	mu      sync.Mutex
	zones   []map[string]any
	records []map[string]any
	nextID  int
	calls   []string
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer tok" {
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 10000, "message": "Authentication error"}}})
		return
	}
	page := func(items []map[string]any, per int) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		p = max(1, p)
		lo, hi := min(len(items), (p-1)*per), min(len(items), p*per)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": items[lo:hi],
			"result_info": map[string]any{"page": p, "total_pages": (len(items) + per - 1) / per}})
	}
	ok := func(v any) { json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v}) }
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	switch {
	case r.URL.Path == "/zones":
		page(f.zones, 2)
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodGet:
		page(f.records, 2)
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodPost:
		f.nextID++
		body["id"] = fmt.Sprint("new", f.nextID)
		f.records = append(f.records, body)
		ok(body)
	default:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		for i, rec := range f.records {
			if rec["id"] != id {
				continue
			}
			if r.Method == http.MethodDelete {
				f.records = append(f.records[:i], f.records[i+1:]...)
			} else {
				for k, v := range body {
					rec[k] = v // PATCH: only the fields sent
				}
			}
			ok(map[string]any{"id": id})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 81044, "message": "Record not found"}}})
	}
}

func TestCloudflare(t *testing.T) {
	fake := &fakeCloudflare{
		zones: []map[string]any{{"id": "z1", "name": "example.com"}, {"id": "z2", "name": "b.org"}, {"id": "z3", "name": "c.net"}},
		records: []map[string]any{
			{"id": "r1", "type": "A", "name": "example.com", "content": "192.0.2.1", "ttl": 1, "proxied": true},
			{"id": "r2", "type": "A", "name": "example.com", "content": "192.0.2.2", "ttl": 1, "proxied": true},
			{"id": "r3", "type": "MX", "name": "example.com", "content": "mx.example.com", "priority": 10, "ttl": 300},
			{"id": "r4", "type": "TXT", "name": "_dmarc.example.com", "content": "v=DMARC1; p=none", "ttl": 300},
			{"id": "r5", "type": "CAA", "name": "example.com", "ttl": 300, "data": map[string]any{"flags": 0, "tag": "issue", "value": "letsencrypt.org"}},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cf := NewCloudflare(config.Source{APIToken: "tok"})
	cf.base = srv.URL
	ctx := context.Background()

	zones, err := cf.ListZones(ctx)
	if err != nil || len(zones) != 3 || zones[2].Name != "c.net" || zones[0].Ref != "z1" {
		t.Fatalf("ListZones (paginated) = %+v, %v", zones, err)
	}
	zone := zones[0]
	recs, err := cf.GetRecords(ctx, zone)
	if err != nil || len(recs) != 5 {
		t.Fatalf("GetRecords = %+v, %v", recs, err)
	}
	want := []string{"A @ 192.0.2.1", "A @ 192.0.2.2", "MX @ 10 mx.example.com", "TXT _dmarc v=DMARC1; p=none", `CAA @ 0 issue "letsencrypt.org"`}
	for i, r := range recs {
		if got := r.Type + " " + r.Host + " " + r.Value; got != want[i] {
			t.Errorf("record %d = %q, want %q", i, got, want[i])
		}
	}

	errs := cf.Apply(ctx, zone, []Change{
		{Kind: store.PendingUpdate, Old: recs[1], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 1}},
		{Kind: store.PendingUpdate, Old: recs[2], New: store.RecordData{Type: "MX", Host: "@", Value: "20 mx2.example.com", TTL: 300}},
		{Kind: store.PendingDelete, Old: recs[3]},
		{Kind: store.PendingCreate, Old: store.RecordData{}, New: store.RecordData{Type: "CNAME", Host: "www", Value: "example.com", TTL: 1}},
		{Kind: store.PendingCreate, New: recs[0]},
		{Kind: store.PendingDelete, Old: store.RecordData{Type: "A", Host: "gone", Value: "192.0.2.7"}},
	})
	for i, err := range errs[:4] {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	if !errors.Is(errs[4], ErrExists) || !errors.Is(errs[5], ErrConflict) {
		t.Errorf("got %v and %v, want ErrExists and ErrConflict", errs[4], errs[5])
	}

	byID := map[string]map[string]any{}
	for _, r := range fake.records {
		byID[fmt.Sprint(r["id"])] = r
	}
	if r := byID["r2"]; r["content"] != "192.0.2.9" || r["proxied"] != true {
		t.Errorf("update must keep proxying: %v", r)
	}
	if r := byID["r3"]; r["content"] != "mx2.example.com" || r["priority"] != float64(20) {
		t.Errorf("MX update: %v", r)
	}
	if _, ok := byID["r4"]; ok {
		t.Errorf("TXT should be deleted")
	}
	if r := byID["new1"]; r["type"] != "CNAME" || r["name"] != "www.example.com" {
		t.Errorf("created record: %v", r)
	}

	bad := NewCloudflare(config.Source{APIToken: "wrong"})
	bad.base = srv.URL
	if _, err := bad.ListZones(ctx); err == nil || !strings.Contains(err.Error(), "Authentication error (10000)") {
		t.Errorf("bad token: %v", err)
	}
}
