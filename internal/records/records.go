// Package records knows the record types the editor offers and how to clean
// up and check a value for each of them.
package records

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/libdns/libdns"
)

// Types are the record types offered by the type selector, most common first.
var Types = []string{"A", "AAAA", "CNAME", "MX", "TXT", "NS", "SRV", "CAA"}

// Example is a sample value per type, shown as a hint.
var Example = map[string]string{
	"A":     "192.0.2.10",
	"AAAA":  "2001:db8::10",
	"CNAME": "target.example.com.",
	"MX":    "10 mx1.example.com.",
	"TXT":   "v=spf1 include:_spf.example.com ~all",
	"NS":    "ns1.example.com.",
	"SRV":   "10 5 443 target.example.com.",
	"CAA":   `0 issue "letsencrypt.org"`,
}

// Normalize trims the value and checks it is valid for the type, returning
// the value to save.
func Normalize(typ, value string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", errors.New("value cannot be empty")
	}
	switch typ {
	case "A", "AAAA":
		ip, err := netip.ParseAddr(v)
		want := "IPv4"
		if typ == "AAAA" {
			want = "IPv6"
		}
		switch {
		case err != nil:
			return v, fmt.Errorf("not an %s address", want)
		case typ == "A" && !ip.Is4():
			return v, errors.New("not an IPv4 address (use AAAA for IPv6)")
		case typ == "AAAA" && (!ip.Is6() || ip.Is4In6()):
			return v, errors.New("not an IPv6 address (use A for IPv4)")
		case ip.Zone() != "":
			return v, fmt.Errorf("an %s address cannot have a zone", want)
		}
		return ip.String(), nil

	case "CNAME", "NS":
		if _, err := netip.ParseAddr(v); err == nil {
			return v, fmt.Errorf("a %s points to a hostname, not an IP address", typ)
		}
		if err := checkHostname(v); err != nil {
			return v, err
		}
		return v, nil

	case "MX":
		f := strings.Fields(v)
		if len(f) != 2 {
			return v, fmt.Errorf("expected \"<priority> <mail server>\", e.g. %s", Example["MX"])
		}
		if _, err := strconv.ParseUint(f[0], 10, 16); err != nil {
			return v, fmt.Errorf("priority %q is not a number from 0 to 65535", f[0])
		}
		if _, err := netip.ParseAddr(f[1]); err == nil {
			return v, errors.New("the mail server must be a hostname, not an IP address")
		}
		if err := checkHostname(f[1]); err != nil {
			return v, err
		}
		return f[0] + " " + f[1], nil

	case "SRV":
		f := strings.Fields(v)
		if len(f) != 4 {
			return v, fmt.Errorf("expected \"<priority> <weight> <port> <target>\", e.g. %s", Example["SRV"])
		}
		for i, name := range []string{"priority", "weight", "port"} {
			if _, err := strconv.ParseUint(f[i], 10, 16); err != nil {
				return v, fmt.Errorf("%s %q is not a number from 0 to 65535", name, f[i])
			}
		}
		if f[3] != "." {
			if err := checkHostname(f[3]); err != nil {
				return v, err
			}
		}
		return strings.Join(f, " "), nil

	case "CAA":
		f := strings.Fields(v)
		if len(f) < 3 {
			return v, fmt.Errorf("expected \"<flags> <tag> <value>\", e.g. %s", Example["CAA"])
		}
		if _, err := strconv.ParseUint(f[0], 10, 8); err != nil {
			return v, fmt.Errorf("flags %q is not a number from 0 to 255", f[0])
		}
		switch strings.ToLower(f[1]) {
		case "issue", "issuewild", "iodef", "issuemail", "issuevmc":
		default:
			return v, fmt.Errorf("unknown CAA tag %q (issue, issuewild or iodef)", f[1])
		}
	}

	rr := libdns.RR{Type: typ, Data: v}
	if _, err := rr.Parse(); err != nil {
		return v, err
	}
	return v, nil
}

// CheckCNAME enforces the CNAME rules for a record of type typ at host
// ("@" for the zone apex): a CNAME must be alone at its name, and is not
// allowed at the apex. others are the types of the other records already at
// that host.
func CheckCNAME(typ, host string, others []string) error {
	name := host
	if host == "" || host == "@" {
		name = "@"
	}
	switch {
	case typ == "CNAME" && name == "@":
		return errors.New("a CNAME isn't allowed at the zone apex (@); use an A record, or your provider's ALIAS")
	case typ == "CNAME" && len(others) > 0:
		return fmt.Errorf("a CNAME can't share its name with other records, and %s already has %s", name, describeTypes(others))
	case typ != "CNAME" && slices.Contains(others, "CNAME"):
		return fmt.Errorf("%s has a CNAME, which can't share its name with other records", name)
	}
	return nil
}

// describeTypes turns ["A", "MX", "A"] into "A and MX records".
func describeTypes(types []string) string {
	var uniq []string
	for _, t := range types {
		if !slices.Contains(uniq, t) {
			uniq = append(uniq, t)
		}
	}
	switch {
	case len(types) == 1:
		return article(types[0]) + " " + types[0] + " record"
	case len(uniq) == 1:
		return uniq[0] + " records"
	default:
		return strings.Join(uniq[:len(uniq)-1], ", ") + " and " + uniq[len(uniq)-1] + " records"
	}
}

// article is "an" for types read letter by letter starting with a vowel
// sound ("an A", "an MX"), else "a" ("a CNAME", "a TXT").
func article(typ string) string {
	if typ != "" && strings.ContainsRune("AEFHILMNORSX", rune(typ[0])) {
		return "an"
	}
	return "a"
}

// checkHostname accepts a relative or absolute (trailing dot) hostname.
// Underscores are allowed, as in service names like _dmarc.
func checkHostname(h string) error {
	name := strings.TrimSuffix(h, ".")
	if name == "" {
		return errors.New("hostname cannot be empty")
	}
	if len(name) > 253 {
		return errors.New("hostname is longer than 253 characters")
	}
	for _, label := range strings.Split(name, ".") {
		switch {
		case label == "":
			return fmt.Errorf("%q has an empty label (two dots in a row?)", h)
		case len(label) > 63:
			return fmt.Errorf("label %q is longer than 63 characters", label)
		case strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-"):
			return fmt.Errorf("label %q cannot start or end with a hyphen", label)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '*') {
				return fmt.Errorf("%q is not a valid hostname (character %q)", h, r)
			}
		}
	}
	return nil
}
