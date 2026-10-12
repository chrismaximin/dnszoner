package records

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Parsed is one line of pasted text, read as a record if it looks like one.
type Parsed struct {
	Line int    // 1-based
	Text string // the line as pasted, trimmed

	// A record when Err and Skip are empty and Type is set.
	Type, Host, Value string
	TTL               int64
	AutoTTL           bool // no TTL given, or "auto"

	Err  error  // looked like a record but isn't valid
	Skip string // a record that is deliberately not added, and why
}

// IsRecord reports whether the line holds a record to add.
func (p Parsed) IsRecord() bool { return p.Type != "" && p.Err == nil && p.Skip == "" }

// Ignored reports whether the line doesn't look like a record at all:
// blank lines, comments, headers, domain names.
func (p Parsed) Ignored() bool { return p.Type == "" && p.Err == nil && p.Skip == "" }

// knownTypes are the types recognised in pasted text; beyond the editor's,
// some are recognised only to be skipped or passed through.
var knownTypes = append(append([]string(nil), Types...),
	"PTR", "SOA", "SPF", "ALIAS", "HTTPS", "SVCB", "DS", "TLSA", "SSHFP", "NAPTR")

var classes = map[string]bool{"IN": true, "CH": true, "HS": true}

// Parse reads records out of pasted text for the zone: dnszoner listings,
// tables split by | or tabs, zone files ("www 300 IN A 192.0.2.1") and copies
// of provider web pages ("A Record  @  192.0.2.1  Automatic").
func Parse(text, zone string) []Parsed {
	var out []Parsed
	defaultTTL, defaultAuto := int64(0), true
	for i, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := cleanLine(raw)
		p := Parsed{Line: i + 1, Text: line}
		switch {
		case line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//"):
		case strings.HasPrefix(strings.ToUpper(line), "$TTL"):
			if f := strings.Fields(line); len(f) == 2 {
				if ttl, auto, err := ParseTTL(f[1]); err == nil {
					defaultTTL, defaultAuto = ttl, auto
				}
			}
		case strings.HasPrefix(line, "$"): // $ORIGIN and other directives
		default:
			p = parseLine(p, zone, defaultTTL, defaultAuto)
		}
		out = append(out, p)
	}
	return out
}

// cleanLine drops tree drawing, list markers and surrounding spaces.
func cleanLine(s string) string {
	s = strings.TrimSpace(s)
	for {
		t := strings.TrimLeftFunc(s, func(r rune) bool {
			return strings.ContainsRune("├└│┌─|*•●✓+>", r) || unicode.IsSpace(r)
		})
		t = strings.TrimPrefix(t, "|-")
		t = strings.TrimPrefix(t, "- ")
		if t == s {
			return s
		}
		s = t
	}
}

// reading is one way of splitting a line into a record.
type reading struct {
	typ, host string
	value     []string
	ttl       string
}

func parseLine(p Parsed, zone string, defaultTTL int64, defaultAuto bool) Parsed {
	// Columns first (tables, listings), then word by word (zone files, which
	// may also be aligned with runs of spaces).
	var readings []reading
	if cols := columns(p.Text); len(cols) > 1 {
		readings = columnReadings(cols)
	}
	readings = append(readings, wordReadings(tokens(p.Text))...)
	if len(readings) == 0 {
		return p // not a record: a header, a domain name...
	}

	var firstErr error
	for _, r := range readings {
		q, err := finish(p, r, zone, defaultTTL, defaultAuto)
		if err == nil {
			return q
		}
		if firstErr == nil {
			firstErr = err
			p.Type = r.typ
		}
	}
	p.Err = firstErr
	return p
}

// finish checks a reading, filling in the record.
func finish(p Parsed, r reading, zone string, defaultTTL int64, defaultAuto bool) (Parsed, error) {
	p.Type = r.typ
	host, err := hostIn(r.host, zone)
	if err != nil {
		return p, err
	}
	p.Host = host

	vals := r.value
	if p.Type == "MX" && len(vals) == 2 && isNumber(vals[1]) && !isNumber(vals[0]) {
		vals = []string{vals[1], vals[0]} // a separate priority column
	}
	value := strings.Join(vals, " ")
	if p.Type == "TXT" || p.Type == "SPF" {
		value = unquoteTXT(value)
	}
	if p.Value, err = Normalize(p.Type, value); err != nil {
		return p, err
	}

	p.TTL, p.AutoTTL = defaultTTL, defaultAuto
	if r.ttl != "" {
		if p.TTL, p.AutoTTL, err = ParseTTL(r.ttl); err != nil {
			return p, err
		}
	}

	switch {
	case p.Type == "SOA":
		p.Skip = "SOA records are managed by the provider"
	case p.Type == "NS" && p.Host == "@":
		p.Skip = "the domain's own NS records are managed by the provider"
	}
	return p, nil
}

// columns splits a line on |, tabs or runs of 2+ spaces, or returns nil when
// it has none of those.
func columns(s string) []string {
	var parts []string
	switch {
	case strings.Contains(s, "|"):
		parts = strings.Split(s, "|")
	case strings.Contains(s, "\t"):
		parts = strings.Split(s, "\t")
	case strings.Contains(s, "  "):
		parts = strings.Split(s, "  ")
	default:
		return nil
	}
	var cols []string
	for _, c := range parts {
		if c = strings.TrimSpace(c); c != "" {
			cols = append(cols, c)
		}
	}
	return cols
}

// columnReadings reads Type Host Value [TTL], or Host [TTL] [class] Type
// Value [TTL].
func columnReadings(cols []string) []reading {
	for i, c := range cols {
		typ, ok := typeName(c)
		if !ok {
			continue
		}
		var r reading
		var rest []string
		if i == 0 {
			if len(cols) < 3 {
				return nil
			}
			r = reading{typ: typ, host: cols[1]}
			rest = cols[2:]
		} else {
			r = reading{typ: typ, host: cols[0]}
			for _, c := range cols[1:i] {
				if isTTL(c) {
					r.ttl = c
				}
			}
			rest = cols[i+1:]
		}
		if len(rest) == 0 {
			return nil
		}
		if len(rest) > 1 && isTTL(rest[len(rest)-1]) {
			r.ttl = rest[len(rest)-1]
			rest = rest[:len(rest)-1]
		}
		r.value = rest
		return []reading{r}
	}
	return nil
}

// wordReadings lists the ways a space-separated line can be read, most
// likely first: "Type host value [ttl]", or zone file style "host [ttl]
// [class] Type value".
func wordReadings(toks []string) []reading {
	var out []reading
	for i, t := range toks {
		typ, ok := typeName(t)
		if !ok {
			continue
		}
		j := i + 1
		if j < len(toks) && strings.EqualFold(toks[j], "record") {
			j++ // "A Record @ 192.0.2.1"
		}
		if i == 0 {
			if len(toks) < j+2 {
				continue
			}
			r := reading{typ: typ, host: toks[j], value: toks[j+1:]}
			if n := len(r.value); n > 1 && isTTL(r.value[n-1]) {
				out = append(out, reading{typ: typ, host: r.host, value: r.value[:n-1], ttl: r.value[n-1]})
			}
			out = append(out, r)
			continue
		}
		r := reading{typ: typ, host: toks[0]}
		prefixOK := true
		for _, p := range toks[1:i] {
			switch {
			case isTTL(p):
				r.ttl = p
			case classes[strings.ToUpper(p)]:
			default:
				prefixOK = false
			}
		}
		if !prefixOK || j >= len(toks) {
			continue
		}
		r.value = toks[j:]
		if n := len(r.value); r.ttl == "" && n > 1 && isTTL(r.value[n-1]) {
			out = append(out, reading{typ: typ, host: r.host, value: r.value[:n-1], ttl: r.value[n-1]})
		}
		out = append(out, r)
	}
	return out
}

// tokens splits on spaces, keeping "quoted strings" whole.
func tokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case unicode.IsSpace(r) && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func typeName(s string) (string, bool) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSpace(strings.TrimSuffix(t, " RECORD"))
	for _, k := range knownTypes {
		if t == k {
			return k, true
		}
	}
	return "", false
}

func isNumber(s string) bool {
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

func isTTL(s string) bool {
	_, _, err := ParseTTL(s)
	return err == nil
}

var ttlUnits = map[string]int64{
	"": 1, "s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
	"m": 60, "min": 60, "mins": 60, "minute": 60, "minutes": 60,
	"h": 3600, "hr": 3600, "hrs": 3600, "hour": 3600, "hours": 3600,
	"d": 86400, "day": 86400, "days": 86400,
	"w": 604800, "week": 604800, "weeks": 604800,
}

// ParseTTL reads seconds, 30s, 5m, 1h, 1d, 1w, "30 min", "1 hour", or
// auto/automatic.
func ParseTTL(s string) (ttl int64, auto bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "auto") || strings.EqualFold(s, "automatic") {
		return 0, true, nil
	}
	i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if i < 0 {
		i = len(s)
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	mult, ok := ttlUnits[strings.ToLower(strings.TrimSpace(s[i:]))]
	if err != nil || !ok {
		return 0, false, fmt.Errorf("invalid TTL %q (seconds, 5m, 1h, 1d or auto)", s)
	}
	return n * mult, false, nil
}

// NormalizeHost turns "www.example.com", "example.com." or "" into the
// zone-relative form ("www", "@").
func NormalizeHost(h, zone string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	switch {
	case h == "" || h == "@" || h == zone:
		return "@"
	case strings.HasSuffix(h, "."+zone):
		return strings.TrimSuffix(h, "."+zone)
	}
	return h
}

// hostIn normalizes a pasted host for the zone, refusing absolute names
// (with a trailing dot) of other domains.
func hostIn(h, zone string) (string, error) {
	n := NormalizeHost(h, zone)
	if strings.HasSuffix(strings.TrimSpace(h), ".") && n != "@" && strings.Contains(n, ".") &&
		!strings.HasSuffix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."), "."+zone) {
		return "", fmt.Errorf("%s is not in %s", strings.TrimSuffix(h, "."), zone)
	}
	if err := checkHostname(strings.ReplaceAll(n, "@", "x")); err != nil {
		return "", errors.New("invalid host: " + err.Error())
	}
	return n, nil
}

// unquoteTXT turns `"v=spf1 -all"` or `"part one" "part two"` into the
// plain text; unquoted values are kept as they are.
func unquoteTXT(v string) string {
	if !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) || len(v) < 2 {
		return v
	}
	return UnquoteTXT(v)
}
