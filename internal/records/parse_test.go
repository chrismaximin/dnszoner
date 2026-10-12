package records

import (
	"fmt"
	"strings"
	"testing"
)

// summary renders the parsed lines compactly for comparison.
func summary(ps []Parsed) string {
	var lines []string
	for _, p := range ps {
		switch {
		case p.Ignored():
			continue
		case p.Err != nil:
			lines = append(lines, fmt.Sprintf("%d ERR %s: %v", p.Line, p.Type, p.Err))
		case p.Skip != "":
			lines = append(lines, fmt.Sprintf("%d SKIP %s %s", p.Line, p.Type, p.Host))
		default:
			ttl := "auto"
			if !p.AutoTTL {
				ttl = fmt.Sprint(p.TTL)
			}
			lines = append(lines, fmt.Sprintf("%d %s %s %s %s", p.Line, p.Type, p.Host, p.Value, ttl))
		}
	}
	return strings.Join(lines, "\n")
}

func check(t *testing.T, text, zone, want string) {
	t.Helper()
	if got := summary(Parse(text, zone)); got != strings.TrimSpace(want) {
		t.Errorf("Parse:\n%s\nwant:\n%s", got, strings.TrimSpace(want))
	}
}

func TestParseListing(t *testing.T) {
	// Copied from the dnszoner results list.
	check(t, `
    Type   Host                             Value                                                      TTL
 example.net [AWS prod]
 ├─ CNAME  notify                           example.net.                                                                                                                   Auto
 ├─ CNAME  www                              example.net.                                                                                                                   30m
 ├─ MX     @                                10 us1-smtp.messagingengine.com.                                                                                                Auto
 ├─ MX     @                                20 us2-smtp.messagingengine.com.                                                                                                Auto
 ├─ MX     notify                           10 feedback-smtp.eu-north-1.amazonses.com.                                                                                      Auto
 ├─ TXT    @                                v=spf1 include:spf.messagingengine.com ?all                                                                                     Auto
 ├─●TXT    _dmarc                           v=DMARC1; p=none;                                                                                                               Auto
 └─ TXT    notify                           v=spf1 include:amazonses.com ~all                                                                                               Auto
`, "example.net", `
4 CNAME notify example.net. auto
5 CNAME www example.net. 1800
6 MX @ 10 us1-smtp.messagingengine.com. auto
7 MX @ 20 us2-smtp.messagingengine.com. auto
8 MX notify 10 feedback-smtp.eu-north-1.amazonses.com. auto
9 TXT @ v=spf1 include:spf.messagingengine.com ?all auto
10 TXT _dmarc v=DMARC1; p=none; auto
11 TXT notify v=spf1 include:amazonses.com ~all auto
`)
}

func TestParseZoneFile(t *testing.T) {
	check(t, `
$ORIGIN example.com.
$TTL 3600
; a comment
@        IN SOA ns1.example.com. admin.example.com. 1 7200 3600 1209600 3600
@        IN NS  ns1.example.com.
@           A     192.0.2.1
www   300 IN CNAME example.com.
a        IN A     192.0.2.2
mail.example.com. IN MX 10 mx.example.com.
_dmarc   IN TXT   "v=DMARC1; " "p=none"
other.org. IN A 192.0.2.3
`, "example.com", `
5 SKIP SOA @
6 SKIP NS @
7 A @ 192.0.2.1 3600
8 CNAME www example.com. 300
9 A a 192.0.2.2 3600
10 MX mail 10 mx.example.com. 3600
11 TXT _dmarc v=DMARC1; p=none 3600
12 ERR A: other.org is not in example.com
`)
}

func TestParseTablesAndProviderCopies(t *testing.T) {
	check(t, `
| Type | Host | Value | TTL
| A | developers | 1.2.3.4 | Auto
A Record	@	192.0.2.1	Automatic
CNAME Record	www	example.com.	30 min
MX Record	@	mx1.example.com.	10	Automatic
A www 192.0.2.9 300
A Record @ 192.0.2.10
AAAA  v6  2001:DB8::1
A  bad  1.2.3
`, "example.com", `
3 A developers 1.2.3.4 auto
4 A @ 192.0.2.1 auto
5 CNAME www example.com. 1800
6 MX @ 10 mx1.example.com. auto
7 A www 192.0.2.9 300
8 A @ 192.0.2.10 auto
9 AAAA v6 2001:db8::1 auto
10 ERR A: not an IPv4 address
`)
}
