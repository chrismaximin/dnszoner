package dns

import (
	"context"
	"encoding/json"
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

// fakeDigitalOcean serves the domains API, one domain per page to exercise
// the paging.
type fakeDigitalOcean struct {
	mu      sync.Mutex
	url     string
	records []map[string]any
	nextID  int
}

func (f *fakeDigitalOcean) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		send(map[string]any{"id": "Unauthorized", "message": "Unable to authenticate you"})
		return
	}
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(p) == 2: // v2/domains
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(1, page)
		names := []string{"example.com", "b.org"}
		links := map[string]any{"pages": map[string]any{}}
		if page < len(names) {
			links["pages"] = map[string]any{"next": f.url + "/v2/domains?page=" + strconv.Itoa(page+1), "last": f.url + "/v2/domains?page=2"}
		}
		send(map[string]any{"domains": []any{map[string]any{"name": names[page-1]}}, "links": links, "meta": map[string]any{"total": 2}})
	case len(p) == 4 && r.Method == http.MethodGet:
		send(map[string]any{"domain_records": f.records, "links": map[string]any{}, "meta": map[string]any{"total": len(f.records)}})
	case len(p) == 4 && r.Method == http.MethodPost:
		f.nextID++
		body["id"] = float64(100 + f.nextID)
		f.records = append(f.records, body)
		send(map[string]any{"domain_record": body})
	case len(p) == 5:
		id, _ := strconv.Atoi(p[4])
		for i, rec := range f.records {
			if int(rec["id"].(float64)) != id {
				continue
			}
			if r.Method == http.MethodDelete {
				f.records = append(f.records[:i], f.records[i+1:]...)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			body["id"] = float64(id)
			f.records[i] = body
			send(map[string]any{"domain_record": body})
			return
		}
		w.WriteHeader(http.StatusNotFound)
		send(map[string]any{"id": "not_found", "message": "The resource you were accessing could not be found."})
	}
}

func TestDigitalOcean(t *testing.T) {
	fake := &fakeDigitalOcean{records: []map[string]any{
		{"id": float64(1), "type": "A", "name": "@", "data": "192.0.2.1", "ttl": float64(1800)},
		{"id": float64(2), "type": "A", "name": "@", "data": "192.0.2.2", "ttl": float64(1800)},
		{"id": float64(3), "type": "MX", "name": "@", "data": "mx.example.com.", "priority": float64(10), "ttl": float64(3600)},
		{"id": float64(4), "type": "SRV", "name": "_sip._tcp", "data": "sip.example.com.", "priority": float64(1), "weight": float64(5), "port": float64(5060), "ttl": float64(3600)},
		{"id": float64(5), "type": "CAA", "name": "@", "data": "letsencrypt.org", "flags": float64(0), "tag": "issue", "ttl": float64(3600)},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	fake.url = srv.URL
	do := NewDigitalOcean(config.Source{APIToken: "tok"})
	do.baseURL = srv.URL + "/"
	ctx := context.Background()

	zones, err := do.ListZones(ctx)
	if err != nil || len(zones) != 2 || zones[1].Name != "b.org" {
		t.Fatalf("ListZones (paginated) = %+v, %v", zones, err)
	}
	recs, err := do.GetRecords(ctx, zones[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Type+" "+r.Host+" "+r.Value)
	}
	want := `A @ 192.0.2.1|A @ 192.0.2.2|MX @ 10 mx.example.com.|SRV _sip._tcp 1 5 5060 sip.example.com.|CAA @ 0 issue "letsencrypt.org"`
	if strings.Join(got, "|") != want {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}

	errs := do.Apply(ctx, zones[0], []Change{
		{Kind: store.PendingUpdate, Old: recs[1], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 1800}},
		{Kind: store.PendingUpdate, Old: recs[2], New: store.RecordData{Type: "MX", Host: "@", Value: "20 mx2.example.com.", TTL: 3600}},
		{Kind: store.PendingDelete, Old: recs[3]},
		{Kind: store.PendingCreate, New: store.RecordData{Type: "TXT", Host: "_dmarc", Value: "v=DMARC1; p=none"}},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	byID := map[float64]map[string]any{}
	for _, r := range fake.records {
		byID[r["id"].(float64)] = r
	}
	if byID[1]["data"] != "192.0.2.1" || byID[2]["data"] != "192.0.2.9" {
		t.Errorf("A records: %v, %v", byID[1], byID[2])
	}
	if mx := byID[3]; mx["data"] != "mx2.example.com." || mx["priority"] != float64(20) {
		t.Errorf("MX: %v", mx)
	}
	if _, ok := byID[4]; ok {
		t.Error("SRV should be deleted")
	}
	if txt := byID[101]; txt["data"] != "v=DMARC1; p=none" || txt["ttl"] != float64(1800) {
		t.Errorf("created TXT: %v", txt)
	}

	bad := NewDigitalOcean(config.Source{APIToken: "wrong"})
	bad.baseURL = srv.URL + "/"
	if _, err := bad.ListZones(ctx); err == nil || !strings.Contains(err.Error(), "Unable to authenticate") {
		t.Errorf("bad token: %v", err)
	}
}
