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

// fakeGoDaddy keeps records by "TYPE name" set, like the GoDaddy API.
type fakeGoDaddy struct {
	mu   sync.Mutex
	sets map[string][]map[string]any
}

func (f *fakeGoDaddy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "sso-key k:s" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"code": "UNABLE_TO_AUTHENTICATE", "message": "Unauthorized"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/domains"), "/")
	switch {
	case r.URL.Path == "/v1/domains":
		json.NewEncoder(w).Encode([]map[string]string{{"domain": "Example.com"}, {"domain": "b.org"}})
	case len(parts) == 3 && r.Method == http.MethodGet: // /example.com/records
		var all []map[string]any
		keys := make([]string, 0, len(f.sets))
		for k := range f.sets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			all = append(all, f.sets[k]...)
		}
		json.NewEncoder(w).Encode(all)
	case len(parts) == 5: // /example.com/records/TYPE/name
		key := parts[3] + " " + parts[4]
		if r.Method == http.MethodDelete {
			delete(f.sets, key)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var body []map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &body)
		for _, rec := range body {
			rec["type"], rec["name"] = parts[3], parts[4]
		}
		f.sets[key] = body
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestGoDaddy(t *testing.T) {
	fake := &fakeGoDaddy{sets: map[string][]map[string]any{
		"A @":     {{"type": "A", "name": "@", "data": "192.0.2.1", "ttl": 600}, {"type": "A", "name": "@", "data": "192.0.2.2", "ttl": 600}},
		"MX @":    {{"type": "MX", "name": "@", "data": "mx.example.com", "priority": 10, "ttl": 3600}},
		"TXT old": {{"type": "TXT", "name": "old", "data": "x", "ttl": 3600}},
		"SRV @":   {{"type": "SRV", "name": "@", "service": "_sip", "protocol": "_tcp", "data": "sip.example.com", "priority": 1, "weight": 5, "port": 5060, "ttl": 3600}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := NewGoDaddy(config.Source{APIKey: "k", APISecret: "s"})
	g.base, g.limiter = srv.URL, newLimiter(0)
	ctx := context.Background()

	zones, err := g.ListZones(ctx)
	if err != nil || len(zones) != 2 || zones[0].Name != "example.com" {
		t.Fatalf("ListZones = %+v, %v", zones, err)
	}
	recs, err := g.GetRecords(ctx, zones[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Type+" "+r.Host+" "+r.Value)
	}
	want := "A @ 192.0.2.1|A @ 192.0.2.2|MX @ 10 mx.example.com|SRV _sip._tcp 1 5 5060 sip.example.com|TXT old x"
	if strings.Join(got, "|") != want {
		t.Fatalf("records:\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}

	// Changing one of two A records must keep the other.
	errs := g.Apply(ctx, zones[0], []Change{
		{Kind: store.PendingUpdate, Old: recs[0], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 600}},
		{Kind: store.PendingUpdate, Old: recs[2], New: store.RecordData{Type: "MX", Host: "@", Value: "20 mx2.example.com", TTL: 3600}},
		{Kind: store.PendingDelete, Old: recs[4]},
		{Kind: store.PendingCreate, New: store.RecordData{Type: "CNAME", Host: "www", Value: "@", TTL: 3600}},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	a := fake.sets["A @"]
	if len(a) != 2 || a[0]["data"] != "192.0.2.2" || a[1]["data"] != "192.0.2.9" {
		t.Errorf("A set = %v", a)
	}
	if mx := fake.sets["MX @"]; len(mx) != 1 || mx[0]["data"] != "mx2.example.com" || mx[0]["priority"] != float64(20) {
		t.Errorf("MX set = %v", mx)
	}
	if _, ok := fake.sets["TXT old"]; ok {
		t.Error("TXT old should be deleted")
	}
	if c := fake.sets["CNAME www"]; len(c) != 1 || c[0]["data"] != "@" {
		t.Errorf("CNAME set = %v", c)
	}

	errs = g.Apply(ctx, zones[0], []Change{{Kind: store.PendingCreate, New: store.RecordData{Type: "TXT", Host: "t", Value: "x", TTL: 300}}})
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "600") {
		t.Errorf("short TTL: %v", errs[0])
	}

	bad := NewGoDaddy(config.Source{APIKey: "k", APISecret: "wrong"})
	bad.base, bad.limiter = srv.URL, newLimiter(0)
	if _, err := bad.ListZones(ctx); err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Errorf("bad key: %v", err)
	}
}
