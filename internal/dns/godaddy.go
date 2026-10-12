package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

const godaddyAPI = "https://api.godaddy.com"

// GoDaddy allows 60 API calls per minute.
const godaddyCallInterval = 1050 * time.Millisecond

// GoDaddy talks to the GoDaddy API directly: its record sets are replaced
// with one call each, keyed by type and name.
type GoDaddy struct {
	src     config.Source
	base    string
	http    *http.Client
	limiter *limiter
}

func NewGoDaddy(src config.Source) *GoDaddy {
	return &GoDaddy{src: src, base: godaddyAPI, http: http.DefaultClient, limiter: newLimiter(godaddyCallInterval)}
}

type gdRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Data     string `json:"data"`
	TTL      int64  `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Weight   *int   `json:"weight,omitempty"`
	Port     *int   `json:"port,omitempty"`
	Service  string `json:"service,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

func (g *GoDaddy) do(ctx context.Context, method, path string, body, out any) error {
	if err := g.limiter.wait(ctx, 1); err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "sso-key "+g.src.APIKey+":"+g.src.APISecret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Code, Message string }
		if json.Unmarshal(raw, &e) == nil && e.Message != "" {
			return fmt.Errorf("godaddy: %s (%s)", e.Message, e.Code)
		}
		return fmt.Errorf("godaddy: %s: %s", resp.Status, truncateBody(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("godaddy: decoding the response: %w", err)
		}
	}
	return nil
}

func (g *GoDaddy) ListZones(ctx context.Context) ([]store.Zone, error) {
	const limit = 1000
	var zones []store.Zone
	marker := ""
	for {
		var res []struct{ Domain string }
		q := url.Values{"statuses": {"ACTIVE"}, "limit": {strconv.Itoa(limit)}}
		if marker != "" {
			q.Set("marker", marker)
		}
		if err := g.do(ctx, http.MethodGet, "/v1/domains?"+q.Encode(), nil, &res); err != nil {
			return nil, err
		}
		for _, d := range res {
			name := strings.ToLower(d.Domain)
			zones = append(zones, store.Zone{Name: name, Ref: name})
		}
		if len(res) < limit {
			return zones, nil
		}
		marker = res[len(res)-1].Domain
	}
}

func (g *GoDaddy) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	return godaddySets{g, zone.Name}.Get(ctx)
}

func (g *GoDaddy) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applySets(ctx, godaddySets{g, zone.Name}, chs)
}

// GoDaddy's default TTL is an hour, and it refuses TTLs under 600 seconds.
func (g *GoDaddy) AutoTTL() int64           { return 3600 }
func (g *GoDaddy) IsAutoTTL(ttl int64) bool { return false }
func (g *GoDaddy) Concurrency() int         { return 1 }

// godaddySets is a zone's records through the GoDaddy API.
type godaddySets struct {
	g    *GoDaddy
	zone string
}

func (s godaddySets) path(typ, name string) string {
	p := "/v1/domains/" + url.PathEscape(s.zone) + "/records"
	if typ != "" {
		p += "/" + url.PathEscape(typ) + "/" + url.PathEscape(name)
	}
	return p
}

func (s godaddySets) Get(ctx context.Context) ([]store.RecordData, error) {
	var res []gdRecord
	if err := s.g.do(ctx, http.MethodGet, s.path("", ""), nil, &res); err != nil {
		return nil, err
	}
	out := make([]store.RecordData, 0, len(res))
	for _, r := range res {
		out = append(out, gdToRecord(r))
	}
	return out, nil
}

func gdToRecord(r gdRecord) store.RecordData {
	d := store.RecordData{Type: r.Type, Host: r.Name, Value: r.Data, TTL: r.TTL}
	n := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	switch r.Type {
	case "MX":
		d.Value = fmt.Sprintf("%d %s", n(r.Priority), r.Data)
	case "SRV":
		d.Host = r.Service + "." + r.Protocol
		if r.Name != "@" && r.Name != "" {
			d.Host += "." + r.Name
		}
		d.Value = fmt.Sprintf("%d %d %d %s", n(r.Priority), n(r.Weight), n(r.Port), r.Data)
	}
	return d
}

// group splits records into their (type, host) sets, in order.
func group(recs []store.RecordData) [][]store.RecordData {
	var keys []string
	sets := map[string][]store.RecordData{}
	for _, r := range recs {
		k := r.Type + " " + r.Host
		if _, ok := sets[k]; !ok {
			keys = append(keys, k)
		}
		sets[k] = append(sets[k], r)
	}
	out := make([][]store.RecordData, len(keys))
	for i, k := range keys {
		out[i] = sets[k]
	}
	return out
}

var errGoDaddySRV = errors.New("changing SRV records isn't supported with GoDaddy yet")

func (s godaddySets) Set(ctx context.Context, recs []store.RecordData) error {
	for _, set := range group(recs) {
		typ, host := set[0].Type, set[0].Host
		if typ == "SRV" {
			return errGoDaddySRV
		}
		// PUT replaces the whole set; its records leave out type and name.
		type putRecord struct {
			Data     string `json:"data"`
			TTL      int64  `json:"ttl"`
			Priority *int   `json:"priority,omitempty"`
		}
		put := make([]putRecord, 0, len(set))
		for _, r := range set {
			if r.TTL < 600 {
				return fmt.Errorf("GoDaddy's shortest TTL is 600 seconds (10m), not %d", r.TTL)
			}
			pr := putRecord{Data: r.Value, TTL: r.TTL}
			if typ == "MX" {
				f := strings.Fields(r.Value)
				if len(f) != 2 {
					return fmt.Errorf("invalid MX value %q", r.Value)
				}
				p, _ := strconv.Atoi(f[0])
				pr.Priority, pr.Data = &p, f[1]
			}
			put = append(put, pr)
		}
		if err := s.g.do(ctx, http.MethodPut, s.path(typ, host), put, nil); err != nil {
			return err
		}
	}
	return nil
}

func (s godaddySets) Delete(ctx context.Context, recs []store.RecordData) error {
	for _, set := range group(recs) {
		if set[0].Type == "SRV" {
			return errGoDaddySRV
		}
		if err := s.g.do(ctx, http.MethodDelete, s.path(set[0].Type, set[0].Host), nil, nil); err != nil {
			return err
		}
	}
	return nil
}
