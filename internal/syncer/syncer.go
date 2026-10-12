// Package syncer refreshes the cache from the providers.
package syncer

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chrismaximin/dnszoner/internal/dns"
	"github.com/chrismaximin/dnszoner/internal/store"
)

// Progress is sent after each step of a sync. The channel is closed when the
// sync is over.
type Progress struct {
	Source string
	Zone   string // empty for source-level events
	Done   int
	Total  int
	Err    error
}

// Run syncs every source concurrently, sending progress on events.
func Run(ctx context.Context, st *store.Store, providers map[string]dns.Provider, events chan<- Progress) {
	defer close(events)
	var wg sync.WaitGroup
	for name, prov := range providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			syncSource(ctx, st, name, prov, events)
		}()
	}
	wg.Wait()
}

func syncSource(ctx context.Context, st *store.Store, source string, prov dns.Provider, events chan<- Progress) {
	listed, err := prov.ListZones(ctx)
	if err != nil {
		events <- Progress{Source: source, Err: fmt.Errorf("listing zones: %w", err)}
		return
	}
	zones, err := st.SetZones(source, listed)
	if err != nil {
		events <- Progress{Source: source, Err: err}
		return
	}
	// Refresh the stalest zones first.
	sort.SliceStable(zones, func(i, j int) bool { return zones[i].SyncedAt.Before(zones[j].SyncedAt) })
	events <- Progress{Source: source, Total: len(zones)}

	var (
		mu   sync.Mutex
		done int
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, max(1, prov.Concurrency()))
	for _, z := range zones {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			recs, err := prov.GetRecords(ctx, z)
			if err == nil {
				err = st.SetRecords(z.ID, recs, time.Now())
			}
			mu.Lock()
			done++
			p := Progress{Source: source, Zone: z.Name, Done: done, Total: len(zones), Err: err}
			mu.Unlock()
			if ctx.Err() == nil {
				events <- p
			}
		}()
	}
	wg.Wait()
}
