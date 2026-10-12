package records

import "testing"

func TestReplace(t *testing.T) {
	tests := []struct{ s, find, repl, want string }{
		{"198.51.100.7", "198.51.100.7", "203.0.113.80", "203.0.113.80"},
		{"198.51.100.70", "198.51.100.7", "203.0.113.80", "198.51.100.70"},
		{"198.51.100.17", "51.100.7", "x", "198.51.100.17"},
		{"10.198.51.100.7", "198.51.100.7", "x", "10.x"}, // a dot is a boundary
		{"v=spf1 ip4:198.51.100.7 ip4:198.51.100.77 -all", "198.51.100.7", "203.0.113.80", "v=spf1 ip4:203.0.113.80 ip4:198.51.100.77 -all"},
		{"10 mx1.mailhost.net.", "mailhost.net", "fastmail.com", "10 mx1.fastmail.com."},
		{"mailhost", "mail", "post", "posthost"}, // letters still match inside words
		{"1.2.3.4 1.2.3.4", "1.2.3.4", "5.6.7.8", "5.6.7.8 5.6.7.8"},
		{"11", "1", "2", "11"},
		{"abc", "", "x", "abc"},
	}
	for _, tt := range tests {
		if got := Replace(tt.s, tt.find, tt.repl); got != tt.want {
			t.Errorf("Replace(%q, %q, %q) = %q, want %q", tt.s, tt.find, tt.repl, got, tt.want)
		}
	}
}
