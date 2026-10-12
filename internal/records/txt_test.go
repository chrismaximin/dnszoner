package records

import (
	"strings"
	"testing"
)

func TestTXTQuoting(t *testing.T) {
	long := strings.Repeat("a", 300)
	for _, v := range []string{"v=spf1 -all", `say "hi" \o/`, "", "café", long, strings.Repeat("é", 200)} {
		q := QuoteTXT(v)
		if got := UnquoteTXT(q); got != v {
			t.Errorf("UnquoteTXT(QuoteTXT(%q)) = %q (quoted: %s)", v, got, q)
		}
		for _, part := range strings.Split(q, `" "`) {
			if len(part) > 257 {
				t.Errorf("QuoteTXT(%q) has a string over 255 bytes", v)
			}
		}
	}
	if q := QuoteTXT(long); !strings.HasPrefix(q, `"`+strings.Repeat("a", 255)+`" "`) {
		t.Errorf("long value not split at 255: %s", q[:20])
	}
	cases := map[string]string{
		`"v=DMARC1; " "p=none"`: "v=DMARC1; p=none",
		`"a\"b" "c\\d"`:         `a"bc\d`,
		`"caf\195\169"`:         "café",
		`plain text`:            "plain text",
	}
	for in, want := range cases {
		if got := UnquoteTXT(in); got != want {
			t.Errorf("UnquoteTXT(%s) = %q, want %q", in, got, want)
		}
	}
}
