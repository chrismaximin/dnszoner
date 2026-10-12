package dns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// fakeHetzner serves the zones and RRSets endpoints of the Hetzner Cloud
// API, completing every action at once.
type fakeHetzner struct {
	mu   sync.Mutex
	sets map[string]map[string]any // by "name/TYPE"
}

var hzAction = map[string]any{"id": 1, "command": "x", "status": "success", "progress": 100,
	"started": "2026-01-01T00:00:00Z", "finished": "2026-01-01T00:00:00Z", "resources": []any{}, "error": nil}

var hzMeta = map[string]any{"pagination": map[string]any{"page": 1, "per_page": 50, "previous_page": nil, "next_page": nil, "last_page": 1, "total_entries": 1}}

func (f *fakeHetzner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusUnauthorized)
		send(map[string]any{"error": map[string]any{"code": "unauthorized", "message": "unable to authenticate"}})
		return
	}
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		json.Unmarshal(b, &body)
	}
	p := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(p) == 1 && p[0] == "zones":
		send(map[string]any{"zones": []any{map[string]any{"id": 7, "name": "example.com", "mode": "primary", "ttl": 3600}}, "meta": hzMeta})
	case len(p) == 3 && r.Method == http.MethodGet: // zones/7/rrsets
		keys := make([]string, 0, len(f.sets))
		for k := range f.sets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []any
		for _, k := range keys {
			out = append(out, f.sets[k])
		}
		send(map[string]any{"rrsets": out, "meta": hzMeta})
	case len(p) == 3 && r.Method == http.MethodPost:
		key := body["name"].(string) + "/" + body["type"].(string)
		body["id"], body["zone"] = key, 7
		f.sets[key] = body
		send(map[string]any{"rrset": body, "action": hzAction})
	case len(p) == 5: // zones/7/rrsets/name/TYPE
		key := p[3] + "/" + p[4]
		set, ok := f.sets[key]
		switch {
		case !ok:
			w.WriteHeader(http.StatusNotFound)
			send(map[string]any{"error": map[string]any{"code": "not_found", "message": "rrset not found"}})
		case r.Method == http.MethodDelete:
			delete(f.sets, key)
			send(map[string]any{"action": hzAction})
		default:
			send(map[string]any{"rrset": set})
		}
	case len(p) == 7 && p[6] == "set_records":
		f.sets[p[3]+"/"+p[4]]["records"] = body["records"]
		send(map[string]any{"action": hzAction})
	case len(p) == 7 && p[6] == "change_ttl":
		f.sets[p[3]+"/"+p[4]]["ttl"] = body["ttl"]
		send(map[string]any{"action": hzAction})
	default:
		w.WriteHeader(http.StatusNotFound)
		send(map[string]any{"error": map[string]any{"code": "not_found", "message": r.Method + " " + r.URL.Path}})
	}
}

func TestHetzner(t *testing.T) {
	rec := func(v string) map[string]any { return map[string]any{"value": v} }
	fake := &fakeHetzner{sets: map[string]map[string]any{
		"@/A":      {"id": "@/A", "name": "@", "type": "A", "ttl": nil, "zone": 7, "records": []any{rec("192.0.2.1"), rec("192.0.2.2")}},
		"@/TXT":    {"id": "@/TXT", "name": "@", "type": "TXT", "ttl": 300, "zone": 7, "records": []any{rec(`"v=spf1 " "-all"`)}},
		"old/AAAA": {"id": "old/AAAA", "name": "old", "type": "AAAA", "ttl": 300, "zone": 7, "records": []any{rec("2001:db8::1")}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	h := NewHetzner(config.Source{APIToken: "tok"})
	h.endpoint = srv.URL
	ctx := context.Background()

	zones, err := h.ListZones(ctx)
	if err != nil || len(zones) != 1 || zones[0].Name != "example.com" || zones[0].Ref != "7" {
		t.Fatalf("ListZones = %+v, %v", zones, err)
	}
	recs, err := h.GetRecords(ctx, zones[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Type+" "+r.Host+" "+r.Value)
	}
	if want := "A @ 192.0.2.1|A @ 192.0.2.2|TXT @ v=spf1 -all|AAAA old 2001:db8::1"; strings.Join(got, "|") != want {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}

	errs := h.Apply(ctx, zones[0], []Change{
		{Kind: store.PendingUpdate, Old: recs[0], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 600}},
		{Kind: store.PendingUpdate, Old: recs[2], New: store.RecordData{Type: "TXT", Host: "@", Value: `v=spf1 "x" -all`, TTL: 300}},
		{Kind: store.PendingDelete, Old: recs[3]},
		{Kind: store.PendingCreate, New: store.RecordData{Type: "CNAME", Host: "www", Value: "example.com."}},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	a := fake.sets["@/A"]
	if rs, _ := json.Marshal(a["records"]); string(rs) != `[{"value":"192.0.2.2"},{"value":"192.0.2.9"}]` || a["ttl"] != float64(600) {
		t.Errorf("A set = %s ttl %v", rs, a["ttl"])
	}
	if rs, _ := json.Marshal(fake.sets["@/TXT"]["records"]); string(rs) != `[{"value":"\"v=spf1 \\\"x\\\" -all\""}]` {
		t.Errorf("TXT set = %s", rs)
	}
	if _, ok := fake.sets["old/AAAA"]; ok {
		t.Error("old/AAAA should be deleted")
	}
	if c := fake.sets["www/CNAME"]; c == nil || c["ttl"] != nil {
		t.Errorf("CNAME set = %v (want no TTL: the zone default)", c)
	}

	bad := NewHetzner(config.Source{APIToken: "wrong"})
	bad.endpoint = srv.URL
	if _, err := bad.ListZones(ctx); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("bad token: %v", err)
	}
}
