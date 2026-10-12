package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/records"
	"github.com/chrismaximin/dnszoner/internal/store"
)

const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// Cloudflare talks to the Cloudflare API directly. Records have IDs there,
// so changes are applied one record at a time, and updates only send the
// record's own fields: settings such as proxying are kept.
type Cloudflare struct {
	src  config.Source
	base string
	http *http.Client
}

func NewCloudflare(src config.Source) *Cloudflare {
	return &Cloudflare{src: src, base: cloudflareAPI, http: http.DefaultClient}
}

type cfError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type cfEnvelope struct {
	Success    bool            `json:"success"`
	Errors     []cfError       `json:"errors"`
	Result     json.RawMessage `json:"result"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type cfRecord struct {
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type"`
	Name     string         `json:"name"`
	Content  string         `json:"content,omitempty"`
	TTL      int64          `json:"ttl"`
	Priority *int           `json:"priority,omitempty"`
	Data     map[string]any `json:"data,omitempty"`
}

// do calls the API, decoding the result into out, and returns the number
// of pages for listings.
func (c *Cloudflare) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.src.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var env cfEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("cloudflare: %s: unexpected response (%s)", resp.Status, truncateBody(raw))
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%s (%d)", e.Message, e.Code))
		}
		if len(msgs) == 0 {
			msgs = append(msgs, resp.Status)
		}
		return 0, fmt.Errorf("cloudflare: %s", strings.Join(msgs, "; "))
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return 0, fmt.Errorf("cloudflare: decoding the result: %w", err)
		}
	}
	return env.ResultInfo.TotalPages, nil
}

func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func (c *Cloudflare) ListZones(ctx context.Context) ([]store.Zone, error) {
	var zones []store.Zone
	for page := 1; ; page++ {
		var res []struct{ ID, Name string }
		pages, err := c.do(ctx, http.MethodGet, "/zones?per_page=50&page="+strconv.Itoa(page), nil, &res)
		if err != nil {
			return nil, err
		}
		for _, z := range res {
			zones = append(zones, store.Zone{Name: strings.ToLower(z.Name), Ref: z.ID})
		}
		if page >= pages || len(res) == 0 {
			return zones, nil
		}
	}
}

func (c *Cloudflare) records(ctx context.Context, zone store.Zone) ([]cfRecord, error) {
	var all []cfRecord
	for page := 1; ; page++ {
		var res []cfRecord
		pages, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone.Ref)+"/dns_records?per_page=1000&page="+strconv.Itoa(page), nil, &res)
		if err != nil {
			return nil, err
		}
		all = append(all, res...)
		if page >= pages || len(res) == 0 {
			return all, nil
		}
	}
}

func (c *Cloudflare) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	recs, err := c.records(ctx, zone)
	if err != nil {
		return nil, err
	}
	out := make([]store.RecordData, len(recs))
	for i, r := range recs {
		out[i] = cfToRecord(r, zone.Name)
	}
	return out, nil
}

func cfToRecord(r cfRecord, zone string) store.RecordData {
	d := store.RecordData{Type: r.Type, Host: records.NormalizeHost(r.Name, zone), Value: r.Content, TTL: r.TTL}
	num := func(k string) int64 {
		f, _ := r.Data[k].(float64)
		return int64(f)
	}
	switch r.Type {
	case "MX":
		if r.Priority != nil {
			d.Value = fmt.Sprintf("%d %s", *r.Priority, r.Content)
		}
	case "SRV":
		if r.Data != nil {
			target, _ := r.Data["target"].(string)
			d.Value = fmt.Sprintf("%d %d %d %s", num("priority"), num("weight"), num("port"), target)
		}
	case "CAA":
		if r.Data != nil {
			tag, _ := r.Data["tag"].(string)
			value, _ := r.Data["value"].(string)
			d.Value = fmt.Sprintf("%d %s %q", num("flags"), tag, value)
		}
	}
	return d
}

// cfFromRecord builds the fields Cloudflare expects for a record.
func cfFromRecord(d store.RecordData, zone string) (cfRecord, error) {
	name := zone
	if d.Host != "@" && d.Host != "" {
		name = d.Host + "." + zone
	}
	r := cfRecord{Type: d.Type, Name: name, TTL: max(1, d.TTL)}
	f := strings.Fields(d.Value)
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch d.Type {
	case "MX":
		if len(f) != 2 {
			return r, fmt.Errorf("invalid MX value %q", d.Value)
		}
		p := atoi(f[0])
		r.Priority, r.Content = &p, f[1]
	case "SRV":
		if len(f) != 4 {
			return r, fmt.Errorf("invalid SRV value %q", d.Value)
		}
		r.Data = map[string]any{"priority": atoi(f[0]), "weight": atoi(f[1]), "port": atoi(f[2]), "target": f[3]}
	case "CAA":
		if len(f) < 3 {
			return r, fmt.Errorf("invalid CAA value %q", d.Value)
		}
		value := strings.Join(f[2:], " ")
		if u, err := strconv.Unquote(value); err == nil {
			value = u
		}
		r.Data = map[string]any{"flags": atoi(f[0]), "tag": f[1], "value": value}
	default:
		r.Content = d.Value
	}
	return r, nil
}

func (c *Cloudflare) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applyByID(ctx, cfIDs{c, zone}, chs)
}

// cfIDs is a zone's records through the Cloudflare API.
type cfIDs struct {
	c    *Cloudflare
	zone store.Zone
}

func (z cfIDs) base() string { return "/zones/" + url.PathEscape(z.zone.Ref) + "/dns_records" }

func (z cfIDs) List(ctx context.Context) ([]idRecord, error) {
	recs, err := z.c.records(ctx, z.zone)
	if err != nil {
		return nil, err
	}
	out := make([]idRecord, len(recs))
	for i, r := range recs {
		out[i] = idRecord{ID: r.ID, Data: cfToRecord(r, z.zone.Name)}
	}
	return out, nil
}

func (z cfIDs) Create(ctx context.Context, d store.RecordData) error {
	body, err := cfFromRecord(d, z.zone.Name)
	if err != nil {
		return err
	}
	_, err = z.c.do(ctx, http.MethodPost, z.base(), body, nil)
	return err
}

// Update uses PATCH, which leaves the fields it isn't sent alone (proxied,
// comment, tags).
func (z cfIDs) Update(ctx context.Context, id string, d store.RecordData) error {
	body, err := cfFromRecord(d, z.zone.Name)
	if err != nil {
		return err
	}
	_, err = z.c.do(ctx, http.MethodPatch, z.base()+"/"+url.PathEscape(id), body, nil)
	return err
}

func (z cfIDs) Delete(ctx context.Context, id string) error {
	_, err := z.c.do(ctx, http.MethodDelete, z.base()+"/"+url.PathEscape(id), nil, nil)
	return err
}

// Cloudflare uses a TTL of 1 for "automatic".
func (c *Cloudflare) AutoTTL() int64           { return 1 }
func (c *Cloudflare) IsAutoTTL(ttl int64) bool { return ttl == 1 }
func (c *Cloudflare) Concurrency() int         { return 4 }
