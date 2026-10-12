package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRenameSourceKeepsZones(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSources([]Source{{Name: "old", Provider: "route53"}}); err != nil {
		t.Fatal(err)
	}
	zones, err := st.SetZones("old", []Zone{{Name: "example.com", Ref: "Z1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetRecords(zones[0].ID, []RecordData{{Type: "A", Host: "@", Value: "192.0.2.1", TTL: 300}}, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := st.RenameSource("old", "new"); err != nil {
		t.Fatal(err)
	}
	// What the accounts screen does next: keep only the configured sources.
	if err := st.SetSources([]Source{{Name: "new", Provider: "route53"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Zones()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "new" || got[0].ID != zones[0].ID {
		t.Fatalf("zones after rename: %+v", got)
	}
	recs, err := st.Records()
	if err != nil || len(recs) != 1 {
		t.Fatalf("records after rename: %+v, %v", recs, err)
	}
}
