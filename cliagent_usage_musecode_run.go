// cliagent_usage_musecode_run.go — keeps Muse Code's quota current from the
// runs the agent already drives, at no cost.
//
// Why this exists:
//
//	Muse Code 1.4.14 reports the account's subscription allowance on EVERY
//	`muse exec --json` run, in an ephemeral status record on the
//	`exec.usage` stream — including a run Meta refused with 429:
//
//	  {"stream":{"kind":"exec.usage","id":"<session>"},"record_type":"status",
//	   "payload_type":"run.usage","payload":{"kind":"run_usage",...,
//	   "allowance":{"tier":"Muse Code Power Usage",
//	     "window":{"used_percent":0,"resets_at_ms":1794169407000},
//	     "weekly":{"used_percent":99,"resets_at_ms":1794182400000},
//	     "as_of_ms":1794151407390,"source":"mint"}}}
//
//	The CLI Agents card read Muse's quota only from the Refresh probe
//	(cliagent_usage_musecode_live.go), which spends a turn — and a spent
//	quota refuses that turn too, so no reading lands. On 2026-11-08 the card
//	still said "≥ 47% used", observed 11/6, while Meta refused every turn
//	with "Subscription quota exhausted". Reading the record each run already
//	prints lands the same per-account reading the parser shows, after every
//	chat, voice or Ship turn, and the Refresh probe's cooldown then keeps it
//	instead of spending a turn of its own.
//
// What it never does: overwrite a newer reading (a Refresh probe or a later
// run), make a model call, or read anything but the allowance numbers.
package main

import (
	"encoding/json"
	"strings"
	"time"
)

// museCodeRunUsagePayloadType names the record (stream.kind "exec.usage").
const museCodeRunUsagePayloadType = "run.usage"

// museCodeDefaultWindowMins is the length of Muse Code's session window ("5-hour
// session window"), used when the run record does not name one and the
// account has no earlier reading that did.
const museCodeDefaultWindowMins = 300

// museCodeRunUsageClockSkew bounds how far in the future a record's as_of_ms
// may sit before it is read as now: a host clock running ahead must not make
// the reading outrank every later one.
const museCodeRunUsageClockSkew = time.Minute

// museCodeRunUsageLine is the subset of a `run.usage` record this file reads.
// Percentages are pointers so a missing figure is told apart from 0 % used.
type museCodeRunUsageLine struct {
	PayloadType string `json:"payload_type"`
	Payload     struct {
		Allowance *struct {
			Window struct {
				UsedPercent        *int  `json:"used_percent"`
				ResetsAtMs         int64 `json:"resets_at_ms"`
				WindowDurationMins int   `json:"window_duration_mins"`
			} `json:"window"`
			Weekly struct {
				UsedPercent *int  `json:"used_percent"`
				ResetsAtMs  int64 `json:"resets_at_ms"`
			} `json:"weekly"`
			AsOfMs int64 `json:"as_of_ms"`
		} `json:"allowance"`
	} `json:"payload"`
}

// parseMuseCodeRunUsageLine reads one stdout line. ok=false for anything that
// is not a complete `run.usage` allowance: both figures present and not
// negative, both reset times set. WindowDurationMins is the record's own, or 0
// when it names none (the caller fills it).
func parseMuseCodeRunUsageLine(line string, now time.Time) (museCodeUsageLiveCache, bool) {
	line = strings.TrimSpace(line)
	// Cheap pre-filter: a run prints dozens of records and only this one counts.
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, museCodeRunUsagePayloadType) ||
		!strings.Contains(line, "allowance") {
		return museCodeUsageLiveCache{}, false
	}
	var rec museCodeRunUsageLine
	if json.Unmarshal([]byte(line), &rec) != nil || rec.PayloadType != museCodeRunUsagePayloadType {
		return museCodeUsageLiveCache{}, false
	}
	a := rec.Payload.Allowance
	if a == nil || a.Window.UsedPercent == nil || a.Weekly.UsedPercent == nil ||
		*a.Window.UsedPercent < 0 || *a.Weekly.UsedPercent < 0 ||
		a.Window.ResetsAtMs <= 0 || a.Weekly.ResetsAtMs <= 0 || a.Window.WindowDurationMins < 0 {
		return museCodeUsageLiveCache{}, false
	}
	observed := a.AsOfMs
	if latest := now.Add(museCodeRunUsageClockSkew).UnixMilli(); observed <= 0 || observed > latest {
		observed = now.UnixMilli()
	}
	var out museCodeUsageLiveCache
	out.ObservedAtMs = observed
	out.Window.UsedPercent = *a.Window.UsedPercent
	out.Window.ResetsAtMs = a.Window.ResetsAtMs
	out.Window.WindowDurationMins = a.Window.WindowDurationMins
	out.Weekly.UsedPercent = *a.Weekly.UsedPercent
	out.Weekly.ResetsAtMs = a.Weekly.ResetsAtMs
	return out, true
}

// captureMuseCodeRunUsageLine records the allowance one `muse exec --json`
// line carries, for whoever Muse runs as now. Reports whether it was kept.
func captureMuseCodeRunUsageLine(line string, now time.Time) bool {
	reading, ok := parseMuseCodeRunUsageLine(line, now)
	if !ok {
		return false
	}
	reading.AccountKey = museCodeUsageAccountKey(currentMuseCodeAccountFingerprint())
	return saveMuseCodeUsageLiveIfNewer(reading)
}

// observeMuseCodeRunUsage scans a finished turn's whole stdout (the one-shot
// native manager's ObserveTurn). Every record is offered; only the newest
// survives the save rule.
func observeMuseCodeRunUsage(stdout string, now time.Time) {
	if !strings.Contains(stdout, museCodeRunUsagePayloadType) {
		return
	}
	for _, line := range strings.Split(stdout, "\n") {
		captureMuseCodeRunUsageLine(line, now)
	}
}

// saveMuseCodeUsageLiveIfNewer saves that account's reading unless the cache
// already holds one observed at the same time or later — read, compare and
// write under one lock, so a Refresh probe landing between them is never
// overwritten by an older run. A reading that names no window length keeps
// the account's earlier one, else the 5-hour default.
func saveMuseCodeUsageLiveIfNewer(entry museCodeUsageLiveCache) bool {
	museCodeUsageLiveCacheMu.Lock()
	defer museCodeUsageLiveCacheMu.Unlock()
	if entry.AccountKey == "" || museCodeUsageLiveCachePath() == "" {
		return false
	}
	file := readMuseCodeUsageLiveFileLocked()
	for _, r := range file.Readings {
		if r.AccountKey != entry.AccountKey {
			continue
		}
		if r.ObservedAtMs >= entry.ObservedAtMs {
			return false
		}
		if entry.Window.WindowDurationMins <= 0 {
			entry.Window.WindowDurationMins = r.Window.WindowDurationMins
		}
	}
	if entry.Window.WindowDurationMins <= 0 {
		entry.Window.WindowDurationMins = museCodeDefaultWindowMins
	}
	return writeMuseCodeUsageLiveLocked(file, entry)
}
