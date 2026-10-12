package changelog

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/chrismaximin/dnszoner/internal/store"
)

func TestChangeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "changes.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := store.RecordData{Type: "A", Host: "www", Value: "192.0.2.1", TTL: 300}
	b := store.RecordData{Type: "A", Host: "www", Value: "192.0.2.2", TTL: 300}
	l.Change("AWS prod", "example.com", store.PendingUpdate, a, b, nil)
	l.Change("AWS prod", "example.com", store.PendingCreate, b, b, nil)
	l.Change("AWS prod", "example.com", store.PendingDelete, a, store.RecordData{}, errors.New("boom"))
	l.Close()

	// Reopening appends rather than truncating.
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.Change("Namecheap", "example.org", store.PendingDelete, store.RecordData{Type: "TXT", Host: "@", Value: "x"}, store.RecordData{}, nil)
	l.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", fi.Mode().Perm())
	}

	f, _ := os.Open(path)
	defer f.Close()
	var lines []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("not JSON: %s", sc.Text())
		}
		lines = append(lines, m)
	}
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4", len(lines))
	}
	if lines[0]["msg"] != "change applied" || lines[0]["name"] != "www.example.com" ||
		lines[0]["old"].(map[string]any)["value"] != "192.0.2.1" || lines[0]["new"].(map[string]any)["value"] != "192.0.2.2" {
		t.Errorf("update line: %v", lines[0])
	}
	if _, ok := lines[1]["old"]; ok {
		t.Errorf("create should have no old record: %v", lines[1])
	}
	if lines[2]["level"] != "ERROR" || lines[2]["error"] != "boom" {
		t.Errorf("failed line: %v", lines[2])
	}
	if _, ok := lines[2]["new"]; ok {
		t.Errorf("delete should have no new record: %v", lines[2])
	}
	if lines[3]["name"] != "example.org" || lines[3]["source"] != "Namecheap" {
		t.Errorf("appended line: %v", lines[3])
	}
}
