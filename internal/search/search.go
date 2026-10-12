// Package search ranks records against a fuzzy, multi-term query.
package search

import (
	"sort"
	"strings"

	"github.com/sahilm/fuzzy"
)

type Zone struct {
	Name   string
	Source string
}

type Record struct {
	Zone  int // index into the zones
	Host  string
	Value string
	Type  string
}

// Group is a zone in the results with its matching records (indices into the
// records), most relevant first.
type Group struct {
	Zone    int
	Records []int
	Score   int
}

type Index struct {
	zones   []Zone
	records []Record
	byZone  [][]int

	zoneNames strs
	sources   strs
	names     strs // full record names, e.g. "developers.example.com"
	values    strs
	types     []string
}

type strs []string

func (s strs) String(i int) string { return s[i] }
func (s strs) Len() int            { return len(s) }

func NewIndex(zones []Zone, records []Record) *Index {
	ix := &Index{zones: zones, records: records, byZone: make([][]int, len(zones))}
	for _, z := range zones {
		ix.zoneNames = append(ix.zoneNames, z.Name)
		ix.sources = append(ix.sources, z.Source)
	}
	for i, r := range records {
		ix.byZone[r.Zone] = append(ix.byZone[r.Zone], i)
		zone := zones[r.Zone].Name
		name := zone
		if r.Host != "@" && r.Host != "" {
			name = r.Host + "." + zone
		}
		ix.names = append(ix.names, name)
		ix.values = append(ix.values, r.Value)
		ix.types = append(ix.types, strings.ToLower(r.Type))
	}
	return ix
}

// Search returns the zones and records matching every term of the query.
// An empty query returns everything in index order.
func (ix *Index) Search(query string) []Group {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		groups := make([]Group, len(ix.zones))
		for z := range ix.zones {
			groups[z] = Group{Zone: z, Records: ix.byZone[z]}
		}
		return groups
	}

	const missing = -1 << 30
	recScore := make([]int, len(ix.records))
	zoneScore := make([]int, len(ix.zones))
	for _, t := range terms {
		zs := ix.bestPerItem(t, len(ix.zones), ix.zoneNames, ix.sources)
		rs := ix.bestPerItem(t, len(ix.records), ix.names, ix.values)
		for i, ty := range ix.types {
			if ty == t && rs[i] < 40 {
				rs[i] = 40 // exact record type, e.g. "mx"
			}
		}
		for z := range zoneScore {
			zoneScore[z] = add(zoneScore[z], zs[z], missing)
		}
		for i, r := range ix.records {
			// A term matching the zone (or its source) counts for all its records.
			recScore[i] = add(recScore[i], max(rs[i], zs[r.Zone]), missing)
		}
	}

	var groups []Group
	for z := range ix.zones {
		g := Group{Zone: z, Score: missing}
		for _, i := range ix.byZone[z] {
			if recScore[i] > missing {
				g.Records = append(g.Records, i)
				g.Score = max(g.Score, recScore[i])
			}
		}
		if len(g.Records) == 0 && zoneScore[z] > missing && len(ix.byZone[z]) == 0 {
			g.Score = zoneScore[z]
		}
		if g.Score == missing {
			continue
		}
		sort.SliceStable(g.Records, func(a, b int) bool { return recScore[g.Records[a]] > recScore[g.Records[b]] })
		groups = append(groups, g)
	}
	sort.SliceStable(groups, func(a, b int) bool {
		if groups[a].Score != groups[b].Score {
			return groups[a].Score > groups[b].Score
		}
		return len(ix.zones[groups[a].Zone].Name) < len(ix.zones[groups[b].Zone].Name)
	})
	return groups
}

func add(total, score, missing int) int {
	if total == missing || score == missing {
		return missing
	}
	return total + score
}

// bestPerItem scores the term against each field and keeps the best score
// per item, or -1<<30 when no field matches.
func (ix *Index) bestPerItem(term string, n int, fields ...strs) []int {
	best := make([]int, n)
	for i := range best {
		best[i] = -1 << 30
	}
	for _, f := range fields {
		for _, m := range fuzzy.FindFrom(term, f) {
			s := m.Score + contiguousBonus(term, f[m.Index])
			if s > best[m.Index] {
				best[m.Index] = s
			}
		}
	}
	return best
}

// contiguousBonus favors exact substrings, most of all at a label boundary,
// over letters scattered across the string.
func contiguousBonus(term, s string) int {
	s = strings.ToLower(s)
	i := strings.Index(s, term)
	switch {
	case i < 0:
		return 0
	case i == 0:
		return 30 + 2*len(term)
	case strings.ContainsRune(".-_ ", rune(s[i-1])):
		return 25 + 2*len(term)
	default:
		return 10 + 2*len(term)
	}
}
