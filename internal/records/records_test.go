package records

import (
	"strings"
	"testing"
)

func TestCheckCNAME(t *testing.T) {
	tests := []struct {
		typ, host string
		others    []string
		want      string // substring of the error, empty for none
	}{
		{"CNAME", "www", nil, ""},
		{"CNAME", "@", nil, "zone apex"},
		{"CNAME", "", nil, "zone apex"},
		{"CNAME", "www", []string{"A"}, "www already has an A record"},
		{"CNAME", "www", []string{"A", "A"}, "A records"},
		{"CNAME", "www", []string{"A", "MX", "TXT"}, "A, MX and TXT records"},
		{"CNAME", "www", []string{"CNAME"}, "already has a CNAME record"},
		{"CNAME", "www", []string{"MX"}, "already has an MX record"},
		{"A", "www", []string{"CNAME"}, "www has a CNAME"},
		{"A", "www", []string{"A", "TXT"}, ""},
		{"TXT", "@", []string{"A", "MX"}, ""},
	}
	for _, tt := range tests {
		err := CheckCNAME(tt.typ, tt.host, tt.others)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("CheckCNAME(%s, %s, %v) = %v, want nil", tt.typ, tt.host, tt.others, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("CheckCNAME(%s, %s, %v) = %v, want %q", tt.typ, tt.host, tt.others, err, tt.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		typ, in, want string
		ok            bool
	}{
		{"A", "  1.2.3.4 ", "1.2.3.4", true},
		{"A", "1.2.3", "", false},
		{"A", "2001:db8::1", "", false},
		{"A", "::ffff:1.2.3.4", "", false},
		{"AAAA", " 2001:DB8::1", "2001:db8::1", true},
		{"AAAA", "1.2.3.4", "", false},
		{"CNAME", " www.example.com. ", "www.example.com.", true},
		{"CNAME", "1.2.3.4", "", false},
		{"CNAME", "bad host.com", "", false},
		{"CNAME", "a..b.com", "", false},
		{"CNAME", "-a.com", "", false},
		{"NS", "ns1.example.com.", "ns1.example.com.", true},
		{"MX", " 10   mx1.example.com. ", "10 mx1.example.com.", true},
		{"MX", "mx1.example.com.", "", false},
		{"MX", "70000 mx.example.com.", "", false},
		{"MX", "10 1.2.3.4", "", false},
		{"TXT", "  v=spf1 -all ", "v=spf1 -all", true},
		{"TXT", "   ", "", false},
		{"SRV", "10 5 443  sip.example.com.", "10 5 443 sip.example.com.", true},
		{"SRV", "10 5 sip.example.com.", "", false},
		{"CAA", `0 issue "letsencrypt.org"`, `0 issue "letsencrypt.org"`, true},
		{"CAA", `0 bogus "x"`, "", false},
		{"SPF", " anything ", "anything", true}, // unknown types are only trimmed
	}
	for _, tt := range tests {
		got, err := Normalize(tt.typ, tt.in)
		if tt.ok && (err != nil || got != tt.want) {
			t.Errorf("Normalize(%s, %q) = %q, %v; want %q", tt.typ, tt.in, got, err, tt.want)
		}
		if !tt.ok && err == nil {
			t.Errorf("Normalize(%s, %q) = %q; want an error", tt.typ, tt.in, got)
		}
	}
}
