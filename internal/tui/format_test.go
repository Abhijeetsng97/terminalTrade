package tui

import "testing"

func TestIndianRupeesGrouping(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "₹0"},
		{999, "₹999"},
		{1000, "₹1,000"},
		{190287, "₹1,90,287"}, // the bug: was ₹01,90,287
		{1234567, "₹12,34,567"},
		{2768681.55, "₹27,68,681.55"},
		{123456789, "₹12,34,56,789"},
		{500, "₹500"},
		{5, "₹5"},
		{5.05, "₹5.05"},
		{12.5, "₹12.50"},
	}
	for _, c := range cases {
		if got := indianRupees(c.in); got != c.want {
			t.Errorf("indianRupees(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
