package records

import "strings"

// Replace replaces find with repl in s, except where a number in find would
// run on into more digits: "192.0.2.7" doesn't match in "192.0.2.70" or
// "10.192.0.2.7", so similar addresses are left alone.
func Replace(s, find, repl string) string {
	if find == "" {
		return s
	}
	var b strings.Builder
	last := 0 // end of what's been copied
	for from := 0; ; {
		i := strings.Index(s[from:], find)
		if i < 0 {
			break
		}
		i += from
		end := i + len(find)
		if isDigit(find[0]) && i > 0 && isDigit(s[i-1]) ||
			isDigit(find[len(find)-1]) && end < len(s) && isDigit(s[end]) {
			from = i + 1 // part of a longer number: look further on
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString(repl)
		last, from = end, end
	}
	b.WriteString(s[last:])
	return b.String()
}
