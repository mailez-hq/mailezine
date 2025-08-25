package directory

import "testing"

func TestSplitDelimited(t *testing.T) {
	cases := []struct {
		addr, delim string
		want        string
		ok          bool
	}{
		{"alice+tag@example.com", "+", "alice@example.com", true},
		{"alice+tag+more@example.com", "+", "alice@example.com", true},
		{"alice@example.com", "+", "", false},
		{"+tag@example.com", "+", "", false},
		{"alice+tag@example.com", "-", "", false},
		{"alice+tag@example.com", "", "", false},
		{"no-at-sign", "+", "", false},
		{"alice@example.com", "", "", false},
		{"", "+", "", false},
	}
	for _, c := range cases {
		got, ok := SplitDelimited(c.addr, c.delim)
		if got != c.want || ok != c.ok {
			t.Errorf("SplitDelimited(%q, %q) = (%q, %v), want (%q, %v)",
				c.addr, c.delim, got, ok, c.want, c.ok)
		}
	}
}
