package dns

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libdns/libdns"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

const namecheapEndpoint = "https://api.namecheap.com/xml.response"

// Namecheap allows 20 API calls per minute.
const namecheapCallInterval = 3100 * time.Millisecond

type Namecheap struct {
	src     config.Source
	limiter *limiter
	http    *http.Client
	httpErr error // an invalid proxy setting

	mu       sync.Mutex
	clientIP string
}

func NewNamecheap(src config.Source) *Namecheap {
	c, err := httpClient(src.Proxy)
	return &Namecheap{src: src, limiter: newLimiter(namecheapCallInterval), http: c, httpErr: err}
}

func (p *Namecheap) endpoint() string {
	if p.src.APIEndpoint != "" {
		return p.src.APIEndpoint
	}
	return namecheapEndpoint
}

// ip returns the whitelisted client IP Namecheap requires on every call.
// Discovered through the proxy when there is one, so it's the proxy's IP.
func (p *Namecheap) ip(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clientIP != "" {
		return p.clientIP, nil
	}
	if p.src.ClientIP != "" {
		p.clientIP = p.src.ClientIP
		return p.clientIP, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("discovering public IP (set client_ip in the config to skip): %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	p.clientIP = strings.TrimSpace(string(b))
	return p.clientIP, nil
}

type ncResponse struct {
	Status string `xml:"Status,attr"`
	Errors []struct {
		Number string `xml:"Number,attr"`
		Text   string `xml:",chardata"`
	} `xml:"Errors>Error"`
}

// call makes one API call, rate limited, and decodes the response into out.
func (p *Namecheap) call(ctx context.Context, command string, params url.Values, out any) error {
	if p.httpErr != nil {
		return p.httpErr
	}
	ip, err := p.ip(ctx)
	if err != nil {
		return err
	}
	if err := p.limiter.wait(ctx, 1); err != nil {
		return err
	}
	q := url.Values{
		"ApiUser":  {p.src.APIUser},
		"ApiKey":   {p.src.APIKey},
		"UserName": {p.src.APIUser},
		"ClientIp": {ip},
		"Command":  {command},
	}
	for k, v := range params {
		q[k] = v
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), strings.NewReader(q.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	var res ncResponse
	if err := xml.Unmarshal(body, &res); err != nil {
		return fmt.Errorf("parsing namecheap response: %w", err)
	}
	if res.Status != "OK" {
		msgs := []string{}
		for _, e := range res.Errors {
			msgs = append(msgs, strings.TrimSpace(e.Text))
		}
		return fmt.Errorf("namecheap: %s", strings.Join(msgs, "; "))
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parsing namecheap response: %w", err)
	}
	return nil
}

type ncDomainList struct {
	Domains []struct {
		Name     string `xml:"Name,attr"`
		IsOurDNS bool   `xml:"IsOurDNS,attr"`
	} `xml:"CommandResponse>DomainGetListResult>Domain"`
	TotalItems int `xml:"CommandResponse>Paging>TotalItems"`
}

func (p *Namecheap) ListZones(ctx context.Context) ([]store.Zone, error) {
	var zones []store.Zone
	for page, seen := 1, 0; ; page++ {
		var res ncDomainList
		params := url.Values{"PageSize": {"100"}, "Page": {strconv.Itoa(page)}}
		if err := p.call(ctx, "namecheap.domains.getList", params, &res); err != nil {
			return nil, err
		}
		for _, d := range res.Domains {
			// Records can only be managed for domains using Namecheap DNS.
			if d.IsOurDNS {
				zones = append(zones, store.Zone{Name: strings.ToLower(d.Name), Ref: strings.ToLower(d.Name)})
			}
		}
		seen += len(res.Domains)
		if len(res.Domains) == 0 || seen >= res.TotalItems {
			break
		}
	}
	return zones, nil
}

// ncHost is a record as getHosts returns it and setHosts takes it.
type ncHost struct {
	Name    string `xml:"Name,attr"`
	Type    string `xml:"Type,attr"`
	Address string `xml:"Address,attr"`
	MXPref  string `xml:"MXPref,attr"`
	TTL     int64  `xml:"TTL,attr"`
}

type ncHosts struct {
	Hosts []ncHost `xml:"CommandResponse>DomainDNSGetHostsResult>host"`
}

// domainParams names a zone as Namecheap wants: the zones are registered
// domains, so the SLD is the first label and the rest is the TLD.
func domainParams(zone string) url.Values {
	sld, tld, _ := strings.Cut(strings.TrimSuffix(zone, "."), ".")
	return url.Values{"SLD": {sld}, "TLD": {tld}}
}

func (p *Namecheap) getHosts(ctx context.Context, zone string) ([]ncHost, error) {
	var res ncHosts
	if err := p.call(ctx, "namecheap.domains.dns.getHosts", domainParams(zone), &res); err != nil {
		return nil, err
	}
	return res.Hosts, nil
}

// setHosts replaces every record of the zone.
func (p *Namecheap) setHosts(ctx context.Context, zone string, hosts []ncHost) error {
	q := domainParams(zone)
	for i, h := range hosts {
		n := strconv.Itoa(i + 1)
		q.Set("HostName"+n, h.Name)
		q.Set("RecordType"+n, h.Type)
		q.Set("Address"+n, h.Address)
		if h.TTL > 0 {
			q.Set("TTL"+n, strconv.FormatInt(h.TTL, 10))
		}
		if strings.EqualFold(h.Type, "MX") {
			q.Set("MXPref"+n, h.MXPref)
			// Custom MX records are only kept with this mail setting.
			q.Set("EmailType", "MX")
		}
	}
	return p.call(ctx, "namecheap.domains.dns.setHosts", q, &ncResponse{})
}

func (p *Namecheap) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	hosts, err := p.getHosts(ctx, zone.Name)
	if err != nil {
		return nil, err
	}
	recs := make([]libdns.Record, 0, len(hosts))
	for _, h := range hosts {
		r, err := h.record()
		if err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return fromLibdns(recs), nil
}

func (p *Namecheap) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	return applySets(ctx, ncSets{p, zone.Name}, chs)
}

// ncSets adapts Namecheap's whole-zone getHosts and setHosts to setAPI:
// setting and deleting are read-modify-write.
type ncSets struct {
	p    *Namecheap
	zone string
}

func (s ncSets) Get(ctx context.Context) ([]store.RecordData, error) {
	return s.p.GetRecords(ctx, store.Zone{Name: s.zone})
}

func (s ncSets) Set(ctx context.Context, recs []store.RecordData) error {
	return s.replace(ctx, recs, recs)
}

func (s ncSets) Delete(ctx context.Context, recs []store.RecordData) error {
	return s.replace(ctx, recs, nil)
}

// replace removes the (host, type) sets of drop from the zone and adds add.
func (s ncSets) replace(ctx context.Context, drop, add []store.RecordData) error {
	hosts, err := s.p.getHosts(ctx, s.zone)
	if err != nil {
		return err
	}
	hosts = slices.DeleteFunc(hosts, func(h ncHost) bool {
		return slices.ContainsFunc(drop, func(r store.RecordData) bool {
			return r.Host == h.Name && strings.EqualFold(r.Type, h.Type)
		})
	})
	for _, r := range add {
		hosts = append(hosts, toNCHost(toLibdns(r)))
	}
	return s.p.setHosts(ctx, s.zone, hosts)
}

// record converts a host to the libdns record its value is formatted from,
// the way the cached records have always been read.
func (h ncHost) record() (libdns.Record, error) {
	ttl := time.Duration(h.TTL) * time.Second
	switch strings.ToUpper(h.Type) {
	case "A", "AAAA":
		ip, err := netip.ParseAddr(h.Address)
		if err != nil {
			return nil, fmt.Errorf("invalid IP address %q: %w", h.Address, err)
		}
		return libdns.Address{Name: h.Name, TTL: ttl, IP: ip}, nil
	case "CNAME":
		return libdns.CNAME{Name: h.Name, TTL: ttl, Target: h.Address}, nil
	case "TXT":
		return libdns.TXT{Name: h.Name, TTL: ttl, Text: h.Address}, nil
	case "MX":
		pref, _ := strconv.Atoi(h.MXPref)
		return libdns.MX{Name: h.Name, TTL: ttl, Preference: uint16(pref), Target: h.Address}, nil
	case "NS":
		return libdns.NS{Name: h.Name, TTL: ttl, Target: h.Address}, nil
	case "CAA":
		// "0 issue letsencrypt.org", the value possibly quoted.
		parts := strings.SplitN(h.Address, " ", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("invalid CAA record %q", h.Address)
		}
		flags, err := strconv.ParseUint(parts[0], 10, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid CAA record %q: %w", h.Address, err)
		}
		return libdns.CAA{Name: h.Name, TTL: ttl, Flags: uint8(flags), Tag: parts[1], Value: unquote(parts[2])}, nil
	}
	return libdns.RR{Name: h.Name, Type: h.Type, Data: h.Address, TTL: ttl}, nil
}

func unquote(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}

// toNCHost converts a record to set. Only MX needs its fields split.
func toNCHost(r libdns.Record) ncHost {
	rr := r.RR()
	h := ncHost{Name: rr.Name, Type: rr.Type, Address: rr.Data, TTL: int64(rr.TTL / time.Second)}
	if mx, ok := r.(libdns.MX); ok {
		// Namecheap silently ignores an MX target without the final dot.
		h.Address = strings.TrimSuffix(mx.Target, ".") + "."
		h.MXPref = strconv.Itoa(int(mx.Preference))
	}
	return h
}

// Namecheap shows a 1799s TTL as "Automatic".
func (p *Namecheap) AutoTTL() int64           { return 1799 }
func (p *Namecheap) IsAutoTTL(ttl int64) bool { return ttl == 1799 }
func (p *Namecheap) Concurrency() int         { return 1 }

// limiter spaces out API calls to stay under a provider's rate limit.
type limiter struct {
	interval time.Duration
	mu       sync.Mutex
	next     time.Time
}

func newLimiter(interval time.Duration) *limiter { return &limiter{interval: interval} }

// wait reserves n call slots and blocks until the first one is due.
func (l *limiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = l.next.Add(time.Duration(n) * l.interval)
	l.mu.Unlock()

	t := time.NewTimer(time.Until(at))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
