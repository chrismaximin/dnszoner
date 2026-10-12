package dns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// fakeCloudDNS serves managed zones, RRSets and changes, refusing changes
// whose deletions don't match the current sets, like Cloud DNS.
type fakeCloudDNS struct {
	mu      sync.Mutex
	sets    map[string]map[string]any // by "name type"
	changes int
}

func (f *fakeCloudDNS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	send := func(v any) { json.NewEncoder(w).Encode(v) }
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		send(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/dns/v1/projects/proj/"), "/")
	switch {
	case len(p) == 1 && p[0] == "managedZones":
		if r.URL.Query().Get("pageToken") == "" {
			send(map[string]any{"managedZones": []any{map[string]any{"name": "example-com", "dnsName": "example.com.", "visibility": "public"}}, "nextPageToken": "p2"})
		} else {
			send(map[string]any{"managedZones": []any{map[string]any{"name": "internal", "dnsName": "corp.internal.", "visibility": "private"}}})
		}
	case len(p) == 3 && p[2] == "rrsets":
		keys := make([]string, 0, len(f.sets))
		for k := range f.sets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []any
		for _, k := range keys {
			out = append(out, f.sets[k])
		}
		send(map[string]any{"rrsets": out})
	case len(p) == 5 && p[2] == "rrsets":
		if set, ok := f.sets[p[3]+" "+p[4]]; ok {
			send(set)
		} else {
			fail(404, "The 'parameters.name' resource named '"+p[3]+"' does not exist.")
		}
	case len(p) == 3 && p[2] == "changes" && r.Method == http.MethodPost:
		var ch struct{ Additions, Deletions []map[string]any }
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &ch)
		for _, d := range ch.Deletions {
			cur := f.sets[d["name"].(string)+" "+d["type"].(string)]
			delete(d, "kind")
			c := map[string]any{}
			for k, v := range cur {
				if k != "kind" {
					c[k] = v
				}
			}
			if !reflect.DeepEqual(c, d) {
				fail(412, "Precondition not met for 'entity.change.deletions[0]'")
				return
			}
		}
		for _, d := range ch.Deletions {
			delete(f.sets, d["name"].(string)+" "+d["type"].(string))
		}
		for _, a := range ch.Additions {
			f.sets[a["name"].(string)+" "+a["type"].(string)] = a
		}
		f.changes++
		send(map[string]any{"id": "c1", "status": "pending"})
	case len(p) == 4 && p[2] == "changes":
		send(map[string]any{"id": p[3], "status": "done"})
	default:
		fail(404, r.Method+" "+r.URL.Path)
	}
}

func TestGoogleCloud(t *testing.T) {
	set := func(name, typ string, ttl int, data ...string) map[string]any {
		d := make([]any, len(data))
		for i, v := range data {
			d[i] = v
		}
		return map[string]any{"name": name, "type": typ, "ttl": float64(ttl), "rrdatas": d}
	}
	fake := &fakeCloudDNS{sets: map[string]map[string]any{
		"example.com. A":        set("example.com.", "A", 300, "192.0.2.1", "192.0.2.2"),
		"example.com. TXT":      set("example.com.", "TXT", 300, `"v=spf1 " "-all"`),
		"old.example.com. AAAA": set("old.example.com.", "AAAA", 300, "2001:db8::1"),
		"geo.example.com. A":    {"name": "geo.example.com.", "type": "A", "ttl": float64(60), "routingPolicy": map[string]any{"geo": map[string]any{}}},
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	gc := NewGoogleCloud(config.Source{Project: "proj"})
	gc.endpoint = srv.URL + "/"
	ctx := context.Background()

	zones, err := gc.ListZones(ctx)
	if err != nil || len(zones) != 2 || zones[0].Name != "example.com" || zones[0].Ref != "example-com" || zones[1].Note != "private" {
		t.Fatalf("ListZones (paginated) = %+v, %v", zones, err)
	}
	recs, err := gc.GetRecords(ctx, zones[0])
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Type+" "+r.Host+" "+r.Value)
	}
	if want := "A @ 192.0.2.1|A @ 192.0.2.2|TXT @ v=spf1 -all|AAAA old 2001:db8::1"; strings.Join(got, "|") != want {
		t.Fatalf("records (routing policy sets skipped):\n%s\nwant:\n%s", strings.Join(got, "|"), want)
	}

	errs := gc.Apply(ctx, zones[0], []Change{
		{Kind: store.PendingUpdate, Old: recs[0], New: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 600}},
		{Kind: store.PendingUpdate, Old: recs[2], New: store.RecordData{Type: "TXT", Host: "@", Value: "v=spf1 ~all", TTL: 300}},
		{Kind: store.PendingDelete, Old: recs[3]},
		{Kind: store.PendingCreate, New: store.RecordData{Type: "CNAME", Host: "www", Value: "example.com."}},
	})
	for i, err := range errs {
		if err != nil {
			t.Errorf("change %d: %v", i, err)
		}
	}
	if a := fake.sets["example.com. A"]; !reflect.DeepEqual(a["rrdatas"], []any{"192.0.2.2", "192.0.2.9"}) || a["ttl"] != float64(600) {
		t.Errorf("A set = %v", a)
	}
	if txt := fake.sets["example.com. TXT"]; !reflect.DeepEqual(txt["rrdatas"], []any{`"v=spf1 ~all"`}) {
		t.Errorf("TXT set = %v", txt)
	}
	if _, ok := fake.sets["old.example.com. AAAA"]; ok {
		t.Error("AAAA should be deleted")
	}
	if c := fake.sets["www.example.com. CNAME"]; c == nil || c["ttl"] != float64(300) {
		t.Errorf("CNAME set = %v (want the default TTL)", c)
	}
	if fake.changes != 2 {
		t.Errorf("got %d changes, want 2 (one for sets, one for deletions)", fake.changes)
	}

	// Deleting one record of a set keeps the others.
	fake.sets["example.com. A"]["rrdatas"] = []any{"192.0.2.2", "192.0.2.9", "192.0.2.77"}
	errs = gc.Apply(ctx, zones[0], []Change{{Kind: store.PendingDelete, Old: store.RecordData{Type: "A", Host: "@", Value: "192.0.2.9", TTL: 600}}})
	if errs[0] != nil {
		t.Errorf("delete one of three: %v", errs[0])
	}
	errs = gc.Apply(ctx, zones[0], []Change{{Kind: store.PendingCreate, New: store.RecordData{Type: "A", Host: "geo", Value: "192.0.2.5", TTL: 60}}})
	if errs[0] == nil || !strings.Contains(errs[0].Error(), "routing policy") {
		t.Errorf("routing policy set: %v", errs[0])
	}
}
