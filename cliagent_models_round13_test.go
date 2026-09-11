package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Codex round 13 on #147: OpenCode's JSON list is deduplicated BEFORE the
// receipt cap, so a repeat among the first entries cannot push a distinct
// later model past the cut and leave a shortened list that reads as complete.

func TestParseOpenCodeModelListDeduplicatesJSONBeforeTheCap(t *testing.T) {
	// 127 distinct ids with three repeats sprinkled in early (130 raw): the
	// cap is never hit, every distinct id survives, and — because the list
	// ends BELOW the cap — a caller may still call it complete.
	var raw []string
	for i := 0; i < 127; i++ {
		raw = append(raw, fmt.Sprintf("provider/model-%03d", i))
		if i < 3 {
			raw = append(raw, "provider/model-000") // early repeats
		}
	}
	body, _ := json.Marshal(raw)
	got := parseOpenCodeModelList(string(body))
	if len(got) != 127 || got[0] != "provider/model-000" || got[126] != "provider/model-126" {
		t.Fatalf("dedupe-then-cap: len=%d first=%q last=%q", len(got), got[0], got[len(got)-1])
	}

	// 129 distinct with the same early repeats (132 raw): capping the RAW
	// slice first would have kept 128 raw entries — 125 distinct — and
	// dropped four listed models while reading as under the cap. Deduplicated
	// first, the list holds 128 distinct ids and sits AT the cap, which the
	// enrichment reports as non-exhaustive.
	raw = raw[:0]
	for i := 0; i < 129; i++ {
		raw = append(raw, fmt.Sprintf("provider/model-%03d", i))
		if i < 3 {
			raw = append(raw, "provider/model-000")
		}
	}
	body, _ = json.Marshal(raw)
	got = parseOpenCodeModelList(string(body))
	if len(got) != cliUsageMaxModelsPerProvider || got[127] != "provider/model-127" {
		t.Fatalf("dedupe-then-cap at the cap: len=%d last=%q", len(got), got[len(got)-1])
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("duplicate survived: %q", id)
		}
		seen[id] = true
	}
}
