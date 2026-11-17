package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The kit's "every point is 0 0" clip-path regex runs in Chrome's backtracking
// engine, so a regex that is linear in Go's RE2 can still hang a page there
// (CodeQL js/redos). This test runs the kit's own literal in Node: it must
// still recognise a zero polygon, and must answer an almost-matching
// clip-path of hostile length at once.
func TestBrowserKitZeroPolygonRegexIsLinear(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	m := regexp.MustCompile(`(?m)^\s*var ZERO_POLYGON = (/.+/);\s*$`).FindStringSubmatch(browserKitJS)
	if m == nil {
		t.Fatal("ZERO_POLYGON not found in the kit")
	}
	script := `
const re = ` + m[1] + `;
const cases = {
  "polygon(0px 0px, 0px 0px, 0px 0px)": true,
  "polygon(0 0,0 0)": true,
  "polygon( 0% 0% , 0px 0 )": true,
  "polygon(0px 0px)": true,
  "polygon(0px 0px, 10px 0px)": false,
  "polygon(0px 0px, 0px 0px,)": false,
  "polygon()": false,
  "inset(0px)": false,
};
const out = {};
for (const [s, want] of Object.entries(cases)) if (re.test(s) !== want) out[s] = !want;
const hostile = "polygon(" + "0 0 ".repeat(5000) + "x)";
const hostileSpaces = "polygon(0 0" + ",0     0".repeat(5000) + " ,";
const t0 = Date.now();
re.test(hostile); re.test(hostileSpaces);
console.log(JSON.stringify({ wrong: out, ms: Date.now() - t0 }));
`
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Stdout only: a Node warning on stderr must not corrupt the JSON result.
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, node, "-e", script)
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("the zero-polygon regex did not finish on a hostile clip-path (catastrophic backtracking)")
	}
	if err != nil {
		t.Fatalf("node: %v: %s", err, stderr.String())
	}
	var res struct {
		Wrong map[string]bool `json:"wrong"`
		Ms    int             `json:"ms"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &res); err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	for s, got := range res.Wrong {
		t.Errorf("ZERO_POLYGON.test(%q) = %v", s, got)
	}
	if res.Ms > 1000 {
		t.Errorf("hostile clip-paths took %d ms; the regex backtracks", res.Ms)
	}
}
