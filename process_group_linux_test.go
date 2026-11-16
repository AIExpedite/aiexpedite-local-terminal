//go:build linux

package main

import "testing"

func TestPgrpFromStat(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		pgrp  int
		state byte
		ok    bool
	}{
		{"123 (cli) S 1 456 456 0 -1", 456, 'S', true},
		{"123 (a) b) Z 1 77 77 0", 77, 'Z', true}, // a ')' inside the command name
		{"123 (cli", 0, 0, false},
		{"123 (cli) S 1", 0, 0, false},
	} {
		g, st, ok := pgrpFromStat([]byte(tc.raw))
		if ok != tc.ok || (ok && (g != tc.pgrp || st != tc.state)) {
			t.Fatalf("pgrpFromStat(%q) = %d %q %v, want %d %q %v", tc.raw, g, st, ok, tc.pgrp, tc.state, tc.ok)
		}
	}
}
