// Package changelog appends every change made to records at a provider to a
// log file, one JSON object per line.
package changelog

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"github.com/chrismaximin/dnszoner/internal/store"
)

// DefaultPath is where changes are logged: the user state directory on
// Linux ($XDG_STATE_HOME, ~/.local/state), ~/Library/Logs on macOS, and the
// local app data directory on Windows.
func DefaultPath(fake bool) string {
	name := "changes.log"
	if fake {
		name = "fake-changes.log"
	}
	return filepath.Join(stateDir(), "dnszoner", name)
}

func stateDir() string {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Logs")
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return d
		}
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return d
	}
	return filepath.Join(home, ".local", "state")
}

type Log struct {
	f    *os.File
	log  *slog.Logger
	Path string
}

// Open opens the log for appending, creating it readable only by the user.
func Open(path string) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, log: slog.New(slog.NewJSONHandler(f, nil)), Path: path}, nil
}

func (l *Log) Close() error { return l.f.Close() }

// Change records the outcome of one change sent to a provider. A nil err
// means it was applied.
func (l *Log) Change(source, zone string, kind store.PendingKind, old, new store.RecordData, err error) {
	if l == nil {
		return
	}
	name := old
	if kind == store.PendingCreate {
		name = new
	}
	attrs := []any{
		slog.String("source", source),
		slog.String("zone", zone),
		slog.String("name", fqdn(name.Host, zone)),
		slog.String("kind", string(kind)),
	}
	if kind != store.PendingCreate {
		attrs = append(attrs, record("old", old))
	}
	if kind != store.PendingDelete {
		attrs = append(attrs, record("new", new))
	}
	if err != nil {
		l.log.Error("change failed", append(attrs, slog.String("error", err.Error()))...)
		return
	}
	l.log.Info("change applied", attrs...)
}

func record(key string, d store.RecordData) slog.Attr {
	return slog.Group(key,
		slog.String("type", d.Type),
		slog.String("host", d.Host),
		slog.String("value", d.Value),
		slog.Int64("ttl", d.TTL),
	)
}

func fqdn(host, zone string) string {
	if host == "" || host == "@" {
		return zone
	}
	return host + "." + zone
}
