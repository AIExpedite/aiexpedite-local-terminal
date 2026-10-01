//go:build linux

package main

import "testing"

func TestParseProcStatParent(t *testing.T) {
	name, ppid, ok := parseProcStatParent([]byte("4242 (my (odd) name) S 17 4242 4242 0 -1 4194560"))
	if !ok || name != "my (odd) name" || ppid != 17 {
		t.Errorf("got (%q,%d,%v), want (\"my (odd) name\",17,true)", name, ppid, ok)
	}
	for _, bad := range []string{"", "4242 no parens S 1", "4242 (agy) S", "4242 (agy) S x"} {
		if _, _, ok := parseProcStatParent([]byte(bad)); ok {
			t.Errorf("parseProcStatParent(%q) ok, want rejected", bad)
		}
	}
}
