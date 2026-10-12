package dns

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/libdns/libdns"
	"github.com/libdns/namecheap"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

const namecheapEndpoint = "https://api.namecheap.com/xml.response"

// Namecheap allows 20 API calls per minute.
const namecheapCallInterval = 3100 * time.Millisecond

type Namecheap struct {
	src     config.Source
	limiter *limiter

	mu       sync.Mutex
	clientIP string
	provider *namecheap.Provider
}

func NewNamecheap(src config.Source) *Namecheap {
	return &Namecheap{src: src, limiter: newLimiter(namecheapCallInterval)}
}

func (p *Namecheap) endpoint() string {
	if p.src.APIEndpoint != "" {
		return p.src.APIEndpoint
	}
	return namecheapEndpoint
}

// ip returns the whitelisted client IP Namecheap requires on every call.
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
	resp, err := http.DefaultClient.Do(req)
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

func (p *Namecheap) libdns(ctx context.Context) (*namecheap.Provider, error) {
	ip, err := p.ip(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider == nil {
		p.provider = &namecheap.Provider{
			APIKey:      p.src.APIKey,
			User:        p.src.APIUser,
			APIEndpoint: p.src.APIEndpoint,
			ClientIP:    ip,
		}
	}
	return p.provider, nil
}

type ncDomainList struct {
	Status string `xml:"Status,attr"`
	Errors []struct {
		Number string `xml:"Number,attr"`
		Text   string `xml:",chardata"`
	} `xml:"Errors>Error"`
	Domains []struct {
		Name     string `xml:"Name,attr"`
		IsOurDNS bool   `xml:"IsOurDNS,attr"`
	} `xml:"CommandResponse>DomainGetListResult>Domain"`
	TotalItems int `xml:"CommandResponse>Paging>TotalItems"`
}

func (p *Namecheap) ListZones(ctx context.Context) ([]store.Zone, error) {
	ip, err := p.ip(ctx)
	if err != nil {
		return nil, err
	}
	var zones []store.Zone
	for page, seen := 1, 0; ; page++ {
		if err := p.limiter.wait(ctx, 1); err != nil {
			return nil, err
		}
		q := url.Values{
			"ApiUser":  {p.src.APIUser},
			"ApiKey":   {p.src.APIKey},
			"UserName": {p.src.APIUser},
			"ClientIp": {ip},
			"Command":  {"namecheap.domains.getList"},
			"PageSize": {"100"},
			"Page":     {strconv.Itoa(page)},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), strings.NewReader(q.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		var res ncDomainList
		if err := xml.Unmarshal(body, &res); err != nil {
			return nil, fmt.Errorf("parsing namecheap response: %w", err)
		}
		if res.Status != "OK" {
			msgs := []string{}
			for _, e := range res.Errors {
				msgs = append(msgs, strings.TrimSpace(e.Text))
			}
			return nil, fmt.Errorf("namecheap: %s", strings.Join(msgs, "; "))
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

func (p *Namecheap) GetRecords(ctx context.Context, zone store.Zone) ([]store.RecordData, error) {
	prov, err := p.libdns(ctx)
	if err != nil {
		return nil, err
	}
	// getHosts, plus the one-time TLD list fetch.
	if err := p.limiter.wait(ctx, 1); err != nil {
		return nil, err
	}
	recs, err := prov.GetRecords(ctx, fqdn(zone.Name))
	if err != nil {
		return nil, err
	}
	return fromLibdns(recs), nil
}

func (p *Namecheap) Apply(ctx context.Context, zone store.Zone, chs []Change) []error {
	prov, err := p.libdns(ctx)
	if err != nil {
		errs := make([]error, len(chs))
		for i := range errs {
			errs[i] = err
		}
		return errs
	}
	return applyLibdns(ctx, limitedAPI{prov, p.limiter}, zone.Name, chs)
}

// limitedAPI reserves rate limit slots before each libdns call. Setting and
// deleting are read-modify-write: getHosts then setHosts.
type limitedAPI struct {
	api     recordAPI
	limiter *limiter
}

func (l limitedAPI) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	if err := l.limiter.wait(ctx, 1); err != nil {
		return nil, err
	}
	return l.api.GetRecords(ctx, zone)
}

func (l limitedAPI) SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if err := l.limiter.wait(ctx, 2); err != nil {
		return nil, err
	}
	return l.api.SetRecords(ctx, zone, recs)
}

func (l limitedAPI) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if err := l.limiter.wait(ctx, 2); err != nil {
		return nil, err
	}
	return l.api.DeleteRecords(ctx, zone, recs)
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
