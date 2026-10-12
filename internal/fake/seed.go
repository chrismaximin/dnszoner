// Package fake fills a store with made-up sources, domains and records for
// trying the app without provider credentials.
package fake

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/chrismaximin/dnszoner/internal/config"
	"github.com/chrismaximin/dnszoner/internal/store"
)

var Sources = []store.Source{
	{Name: "AWS prod", Provider: "route53"},
	{Name: "AWS staging", Provider: "route53"},
	{Name: "Namecheap", Provider: "namecheap"},
	{Name: "Namecheap clients", Provider: "namecheap"},
}

// ConfigSource is a plausible account config for a fake source, so the
// accounts screen has something to show.
func ConfigSource(src store.Source) config.Source {
	c := config.Source{Name: src.Name, Provider: src.Provider}
	switch src.Name {
	case "AWS prod":
		c.Profile = "prod"
	case "AWS staging":
		c.Profile, c.Region = "staging", "eu-west-1"
	default:
		c.APIUser, c.APIKey = "acme", "fake-api-key-0000"
		if src.Name == "Namecheap clients" {
			c.APIUser = "acme-clients"
		}
	}
	return c
}

var (
	prefixes = []string{"apple", "acme", "blue", "bright", "cedar", "cloud", "copper", "crisp", "delta",
		"ember", "fable", "falcon", "fern", "golden", "granite", "harbor", "hollow", "indigo", "iron",
		"juniper", "kestrel", "lumen", "maple", "meadow", "nimbus", "north", "oak", "onyx", "pixel",
		"quartz", "raven", "river", "saffron", "silver", "solar", "spruce", "stone", "swift", "tidal",
		"umber", "velvet", "willow", "zephyr"}
	suffixes = []string{"labs", "works", "studio", "cart", "forge", "hub", "byte", "nest", "craft",
		"field", "box", "line", "path", "point", "stack", "wave", "yard", "mint", "base", "loop"}
	tlds       = []string{"com", "com", "com", "io", "dev", "net", "org", "co", "app", "co.uk", "de", "fr"}
	subdomains = []string{"api", "app", "dev", "developers", "staging", "cdn", "blog", "shop", "status",
		"docs", "admin", "vpn", "git", "ci", "grafana", "auth", "assets", "beta", "portal", "mail",
		"files", "metrics", "support", "help", "m", "img", "static", "sandbox", "preview", "internal"}
	cnameTargets = []string{"d1x2y3z4.cloudfront.net.", "lb-prod-123456.eu-west-1.elb.amazonaws.com.",
		"cname.vercel-dns.com.", "ghs.googlehosted.com.", "proxy.fly.dev.", "custom.netlify.app.",
		"hosted.statuspage.io.", "ext-cust.squarespace.com.", "shops.myshopify.com."}
)

// Seed replaces the store's content with generated data. It is
// deterministic, so the fake data is the same on every machine.
func Seed(st *store.Store) error {
	r := rand.New(rand.NewPCG(42, 2024))
	if err := st.SetSources(nil); err != nil {
		return err
	}
	if err := st.SetSources(Sources); err != nil {
		return err
	}

	used := map[string]bool{}
	now := time.Now()
	for i, src := range Sources {
		count := []int{140, 60, 180, 90}[i]
		var zones []store.Zone
		if i == 0 {
			zones = append(zones, store.Zone{Name: "applepie.io", Ref: "applepie.io"})
			used["applepie.io"] = true
		}
		for len(zones) < count {
			name := fmt.Sprintf("%s%s.%s", pick(r, prefixes), pick(r, suffixes), pick(r, tlds))
			if r.IntN(4) == 0 {
				name = fmt.Sprintf("%s-%s.%s", pick(r, prefixes), pick(r, suffixes), pick(r, tlds))
			}
			if used[name] {
				continue
			}
			used[name] = true
			ref := name
			if src.Provider == "route53" {
				ref = fmt.Sprintf("Z%013d", r.Int64N(1e13))
			}
			zones = append(zones, store.Zone{Name: name, Ref: ref})
		}
		saved, err := st.SetZones(src.Name, zones)
		if err != nil {
			return err
		}
		for _, z := range saved {
			if err := st.SetRecords(z.ID, records(r, src.Provider, z.Name), now); err != nil {
				return err
			}
		}
	}
	return nil
}

func records(r *rand.Rand, provider, zone string) []store.RecordData {
	ttl := func() int64 {
		if provider == "namecheap" {
			if r.IntN(5) == 0 {
				return []int64{60, 300, 3600}[r.IntN(3)]
			}
			return 1799
		}
		return []int64{60, 300, 300, 3600, 86400}[r.IntN(5)]
	}
	ip := func() string {
		return fmt.Sprintf("%s.%d", pick(r, []string{"203.0.113", "198.51.100", "192.0.2"}), 1+r.IntN(254))
	}
	var out []store.RecordData
	add := func(typ, host, value string, ttl int64) {
		out = append(out, store.RecordData{Type: typ, Host: host, Value: value, TTL: ttl})
	}

	if zone == "applepie.io" {
		add("A", "@", "1.2.3.4", 300)
		add("A", "developers", "1.2.3.4", 0)
		add("CNAME", "dev", "developers.applepie.io.", 0)
		add("CNAME", "www", "applepie.io.", 300)
		add("MX", "@", "10 mx1.mailhost.net.", 3600)
		add("TXT", "@", "v=spf1 include:_spf.mailhost.net ~all", 3600)
		return out
	}

	if provider == "route53" {
		n := r.IntN(2000)
		for i := 1; i <= 4; i++ {
			add("NS", "@", fmt.Sprintf("ns-%d.awsdns-%02d.%s.", n+i*7, r.IntN(64), []string{"com", "net", "org", "co.uk"}[i-1]), 172800)
		}
	}
	apexTTL := ttl()
	for range 1 + r.IntN(2) {
		add("A", "@", ip(), apexTTL)
	}
	if r.IntN(3) == 0 {
		add("AAAA", "@", fmt.Sprintf("2001:db8::%x", r.IntN(0xffff)), apexTTL)
	}
	add("CNAME", "www", zone+".", ttl())
	if r.IntN(4) != 0 {
		mxTTL := ttl()
		add("MX", "@", "10 mx1.mailhost.net.", mxTTL)
		add("MX", "@", "20 mx2.mailhost.net.", mxTTL)
		add("TXT", "@", "v=spf1 include:_spf.mailhost.net ~all", ttl())
		add("TXT", "_dmarc", "v=DMARC1; p=quarantine; rua=mailto:dmarc@"+zone, ttl())
	}
	if r.IntN(3) == 0 {
		add("TXT", "@", fmt.Sprintf("google-site-verification=%x", r.Uint64()), ttl())
	}
	if r.IntN(5) == 0 {
		add("CAA", "@", `0 issue "letsencrypt.org"`, ttl())
	}

	seen := map[string]bool{}
	for range r.IntN(12) {
		sub := pick(r, subdomains)
		if seen[sub] {
			continue
		}
		seen[sub] = true
		switch r.IntN(3) {
		case 0:
			add("A", sub, ip(), ttl())
		case 1:
			add("CNAME", sub, pick(r, cnameTargets), ttl())
		default:
			add("CNAME", sub, zone+".", ttl())
		}
	}
	return out
}

func pick[T any](r *rand.Rand, xs []T) T { return xs[r.IntN(len(xs))] }
