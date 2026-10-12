package records

import (
	"strings"
	"unicode/utf8"
)

// QuoteTXT turns a TXT value into zone file form: quoted strings of at most
// 255 bytes, with quotes and backslashes escaped.
func QuoteTXT(v string) string {
	var parts []string
	for len(v) > 0 || len(parts) == 0 {
		n := min(len(v), 255)
		for n < len(v) && n > 0 && !utf8.RuneStart(v[n]) {
			n-- // don't split a character
		}
		chunk := v[:n]
		v = v[n:]
		chunk = strings.ReplaceAll(chunk, `\`, `\\`)
		chunk = strings.ReplaceAll(chunk, `"`, `\"`)
		parts = append(parts, `"`+chunk+`"`)
	}
	return strings.Join(parts, " ")
}

// UnquoteTXT turns zone file TXT strings back into the plain value: the
// quoted strings are joined, and \X and \DDD (decimal) escapes resolved.
// A value without quotes is returned as it is.
func UnquoteTXT(v string) string {
	if !strings.Contains(v, `"`) {
		return v
	}
	var b strings.Builder
	in := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '"':
			in = !in
		case c == '\\' && i+3 < len(v) && isDigit(v[i+1]) && isDigit(v[i+2]) && isDigit(v[i+3]):
			b.WriteByte((v[i+1]-'0')*100 + (v[i+2]-'0')*10 + (v[i+3] - '0'))
			i += 3
		case c == '\\' && i+1 < len(v):
			b.WriteByte(v[i+1])
			i++
		case !in && (c == ' ' || c == '\t'):
			// between strings
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
