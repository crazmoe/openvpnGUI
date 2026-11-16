package main

import "testing"

func TestParseXftDPI(t *testing.T) {
	cases := map[string]float64{
		"Xft.dpi:\t192\nXft.antialias:\t1\n": 192,
		"Xcursor.size: 24\nXft.dpi: 144\n":   144,
		"Xft.antialias:\t1\n":                0,
		"Xft.dpi:\tabc\n":                    0,
		"Xft.dpi:\t5000\n":                   0,
	}
	for in, want := range cases {
		if got := parseXftDPI([]byte(in)); got != want {
			t.Errorf("parseXftDPI(%q) = %v, want %v", in, got, want)
		}
	}
}
