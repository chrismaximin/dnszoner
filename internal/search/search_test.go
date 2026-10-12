package search

import "testing"

func TestSearchRanksHostAndZoneTerms(t *testing.T) {
	zones := []Zone{{Name: "applepie.io", Source: "AWS"}, {Name: "otherdomain.com", Source: "Namecheap"}, {Name: "papaya-dev.net", Source: "AWS"}}
	recs := []Record{
		{Zone: 0, Host: "@", Value: "1.2.3.4", Type: "A"},
		{Zone: 0, Host: "developers", Value: "1.2.3.4", Type: "A"},
		{Zone: 0, Host: "dev", Value: "developers.applepie.io.", Type: "CNAME"},
		{Zone: 1, Host: "developer", Value: "5.6.7.8", Type: "A"},
		{Zone: 2, Host: "www", Value: "papaya-dev.net.", Type: "CNAME"},
	}
	ix := NewIndex(zones, recs)

	got := ix.Search("app devel")
	if len(got) == 0 || got[0].Zone != 0 {
		t.Fatalf("expected applepie.io first, got %+v", got)
	}
	if first := got[0].Records[0]; first != 1 {
		t.Fatalf("expected developers record first, got %d (%+v)", first, recs[first])
	}
	for _, g := range got {
		if g.Zone == 1 {
			t.Fatalf("otherdomain.com should not match 'app': %+v", got)
		}
	}

	if got := ix.Search("5.6.7"); len(got) != 1 || got[0].Zone != 1 {
		t.Fatalf("value search: %+v", got)
	}
	if got := ix.Search(""); len(got) != 3 || len(got[0].Records) != 3 {
		t.Fatalf("empty query should return everything: %+v", got)
	}
	if got := ix.Search("namecheap"); len(got) != 1 || got[0].Zone != 1 {
		t.Fatalf("source search: %+v", got)
	}
}
