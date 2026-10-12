// Package store is the SQLite cache of sources, zones and records, plus the
// queue of changes saved for later.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Source struct {
	Name     string
	Provider string
}

type Zone struct {
	ID       int64
	Source   string
	Name     string // without trailing dot
	Ref      string // provider-specific zone identifier (e.g. Route53 hosted zone ID)
	Note     string // extra label shown next to the zone, e.g. "private"
	SyncedAt time.Time
}

// RecordData is the editable content of a record. Host is relative to the
// zone ("@" for the apex), TTL is in seconds.
type RecordData struct {
	Type  string
	Host  string
	Value string
	TTL   int64
}

type Record struct {
	ID     int64
	ZoneID int64
	RecordData
}

type PendingKind string

const (
	PendingUpdate PendingKind = "update"
	PendingDelete PendingKind = "delete"
)

// Pending is a change saved for later. It is keyed by the record's old
// content rather than its row ID, since syncs recreate record rows.
type Pending struct {
	ID        int64
	ZoneID    int64
	Kind      PendingKind
	Old       RecordData
	New       RecordData // zero for deletes
	CreatedAt time.Time
	LastError string
}

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sources (
	name     TEXT PRIMARY KEY,
	provider TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS zones (
	id        INTEGER PRIMARY KEY,
	source    TEXT NOT NULL REFERENCES sources(name) ON DELETE CASCADE,
	name      TEXT NOT NULL,
	ref       TEXT NOT NULL,
	note      TEXT NOT NULL DEFAULT '',
	synced_at INTEGER NOT NULL DEFAULT 0,
	UNIQUE (source, ref)
);
CREATE TABLE IF NOT EXISTS records (
	id      INTEGER PRIMARY KEY,
	zone_id INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
	type    TEXT NOT NULL,
	host    TEXT NOT NULL,
	value   TEXT NOT NULL,
	ttl     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS records_zone ON records(zone_id);
CREATE TABLE IF NOT EXISTS pending (
	id         INTEGER PRIMARY KEY,
	zone_id    INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
	kind       TEXT NOT NULL,
	old_type   TEXT NOT NULL,
	old_host   TEXT NOT NULL,
	old_value  TEXT NOT NULL,
	old_ttl    INTEGER NOT NULL,
	new_type   TEXT NOT NULL DEFAULT '',
	new_host   TEXT NOT NULL DEFAULT '',
	new_value  TEXT NOT NULL DEFAULT '',
	new_ttl    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	UNIQUE (zone_id, old_type, old_host, old_value)
);
`

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// A single connection keeps writes serialized and pragmas applied.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SetSources makes the sources table match the configured sources, dropping
// the cache of sources that are no longer configured.
func (s *Store) SetSources(sources []Source) error {
	return s.tx(func(tx *sql.Tx) error {
		keep := map[string]bool{}
		for _, src := range sources {
			keep[src.Name] = true
			if _, err := tx.Exec(`INSERT INTO sources(name, provider) VALUES(?, ?)
				ON CONFLICT(name) DO UPDATE SET provider = excluded.provider`, src.Name, src.Provider); err != nil {
				return err
			}
		}
		existing, err := querySources(tx)
		if err != nil {
			return err
		}
		for _, src := range existing {
			if !keep[src.Name] {
				if _, err := tx.Exec(`DELETE FROM sources WHERE name = ?`, src.Name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) Sources() ([]Source, error) { return querySources(s.db) }

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func querySources(q querier) ([]Source, error) {
	rows, err := q.Query(`SELECT name, provider FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.Name, &src.Provider); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// SetZones makes the source's zones match the given list (matched by Ref),
// keeping the IDs and records of zones that still exist. It returns the
// resulting zones.
func (s *Store) SetZones(source string, zones []Zone) ([]Zone, error) {
	err := s.tx(func(tx *sql.Tx) error {
		keep := map[string]bool{}
		for _, z := range zones {
			keep[z.Ref] = true
			if _, err := tx.Exec(`INSERT INTO zones(source, name, ref, note) VALUES(?, ?, ?, ?)
				ON CONFLICT(source, ref) DO UPDATE SET name = excluded.name, note = excluded.note`,
				source, z.Name, z.Ref, z.Note); err != nil {
				return err
			}
		}
		rows, err := tx.Query(`SELECT id, ref FROM zones WHERE source = ?`, source)
		if err != nil {
			return err
		}
		var drop []int64
		for rows.Next() {
			var id int64
			var ref string
			if err := rows.Scan(&id, &ref); err != nil {
				rows.Close()
				return err
			}
			if !keep[ref] {
				drop = append(drop, id)
			}
		}
		rows.Close()
		for _, id := range drop {
			if _, err := tx.Exec(`DELETE FROM zones WHERE id = ?`, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	all, err := s.Zones()
	if err != nil {
		return nil, err
	}
	var out []Zone
	for _, z := range all {
		if z.Source == source {
			out = append(out, z)
		}
	}
	return out, nil
}

func (s *Store) Zones() ([]Zone, error) {
	rows, err := s.db.Query(`SELECT id, source, name, ref, note, synced_at FROM zones ORDER BY name, source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Zone
	for rows.Next() {
		var z Zone
		var synced int64
		if err := rows.Scan(&z.ID, &z.Source, &z.Name, &z.Ref, &z.Note, &synced); err != nil {
			return nil, err
		}
		if synced > 0 {
			z.SyncedAt = time.Unix(synced, 0)
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// SetRecords replaces the cached records of a zone.
func (s *Store) SetRecords(zoneID int64, records []RecordData, syncedAt time.Time) error {
	return s.tx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM records WHERE zone_id = ?`, zoneID); err != nil {
			return err
		}
		stmt, err := tx.Prepare(`INSERT INTO records(zone_id, type, host, value, ttl) VALUES(?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range records {
			if _, err := stmt.Exec(zoneID, r.Type, r.Host, r.Value, r.TTL); err != nil {
				return err
			}
		}
		_, err = tx.Exec(`UPDATE zones SET synced_at = ? WHERE id = ?`, syncedAt.Unix(), zoneID)
		return err
	})
}

func (s *Store) Records() ([]Record, error) {
	rows, err := s.db.Query(`SELECT id, zone_id, type, host, value, ttl FROM records ORDER BY zone_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.ZoneID, &r.Type, &r.Host, &r.Value, &r.TTL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ZoneRecords returns the cached records of one zone.
func (s *Store) ZoneRecords(zoneID int64) ([]RecordData, error) {
	rows, err := s.db.Query(`SELECT type, host, value, ttl FROM records WHERE zone_id = ? ORDER BY id`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecordData
	for rows.Next() {
		var r RecordData
		if err := rows.Scan(&r.Type, &r.Host, &r.Value, &r.TTL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyLocal mirrors a change that was applied at the provider into the cache.
func (s *Store) ApplyLocal(zoneID int64, kind PendingKind, old, new RecordData) error {
	var res sql.Result
	var err error
	switch kind {
	case PendingDelete:
		res, err = s.db.Exec(`DELETE FROM records WHERE id = (
			SELECT id FROM records WHERE zone_id = ? AND type = ? AND host = ? AND value = ? LIMIT 1)`,
			zoneID, old.Type, old.Host, old.Value)
	case PendingUpdate:
		res, err = s.db.Exec(`UPDATE records SET type = ?, host = ?, value = ?, ttl = ? WHERE id = (
			SELECT id FROM records WHERE zone_id = ? AND type = ? AND host = ? AND value = ? LIMIT 1)`,
			new.Type, new.Host, new.Value, new.TTL, zoneID, old.Type, old.Host, old.Value)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				// Providers apply a TTL to the whole (host, type) set.
				_, err = s.db.Exec(`UPDATE records SET ttl = ? WHERE zone_id = ? AND type = ? AND host = ?`,
					new.TTL, zoneID, new.Type, new.Host)
			}
		}
	default:
		return fmt.Errorf("unknown change kind %q", kind)
	}
	return err
}

// SavePending queues a change, replacing any change already queued for the
// same record.
func (s *Store) SavePending(p Pending) error { return s.SavePendings([]Pending{p}) }

// SavePendings saves several changes in one transaction.
func (s *Store) SavePendings(ps []Pending) error {
	return s.tx(func(tx *sql.Tx) error {
		for _, p := range ps {
			if err := savePending(tx, p); err != nil {
				return err
			}
		}
		return nil
	})
}

func savePending(tx *sql.Tx, p Pending) error {
	_, err := tx.Exec(`INSERT INTO pending(zone_id, kind, old_type, old_host, old_value, old_ttl,
			new_type, new_host, new_value, new_ttl, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(zone_id, old_type, old_host, old_value) DO UPDATE SET
			kind = excluded.kind, old_ttl = excluded.old_ttl,
			new_type = excluded.new_type, new_host = excluded.new_host,
			new_value = excluded.new_value, new_ttl = excluded.new_ttl,
			created_at = excluded.created_at, last_error = ''`,
		p.ZoneID, p.Kind, p.Old.Type, p.Old.Host, p.Old.Value, p.Old.TTL,
		p.New.Type, p.New.Host, p.New.Value, p.New.TTL, time.Now().Unix())
	return err
}

func (s *Store) Pending() ([]Pending, error) {
	rows, err := s.db.Query(`SELECT id, zone_id, kind, old_type, old_host, old_value, old_ttl,
		new_type, new_host, new_value, new_ttl, created_at, last_error FROM pending ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pending
	for rows.Next() {
		var p Pending
		var created int64
		if err := rows.Scan(&p.ID, &p.ZoneID, &p.Kind, &p.Old.Type, &p.Old.Host, &p.Old.Value, &p.Old.TTL,
			&p.New.Type, &p.New.Host, &p.New.Value, &p.New.TTL, &created, &p.LastError); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(created, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeletePending(id int64) error {
	_, err := s.db.Exec(`DELETE FROM pending WHERE id = ?`, id)
	return err
}

// DeletePendingFor drops any change queued for the given record.
func (s *Store) DeletePendingFor(zoneID int64, old RecordData) error {
	_, err := s.db.Exec(`DELETE FROM pending WHERE zone_id = ? AND old_type = ? AND old_host = ? AND old_value = ?`,
		zoneID, old.Type, old.Host, old.Value)
	return err
}

func (s *Store) SetPendingError(id int64, msg string) error {
	_, err := s.db.Exec(`UPDATE pending SET last_error = ? WHERE id = ?`, msg, id)
	return err
}

// Empty reports whether the store holds no sources at all.
func (s *Store) Empty() (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM sources`).Scan(&n)
	return n == 0, err
}

func (s *Store) tx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}
