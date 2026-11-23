package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Google marks a bucket the account is not entitled to with "disabled": true.
// Before this was decoded such a bucket became a capacity row — with a literal
// fraction always, and with an omitted one whenever exhaustion evidence named
// its reset, which read "Limit reached" for capacity the account never had.
// These pin the classification: only a literal true disables, a disabled
// bucket is never a row whatever it says, and a reply holding nothing else is
// still an observation when its windows are recognised.

const helperDisabledReset = "2126-11-09T14:22:54Z"

// helperDisabledReply is one Gemini 5h reading beside one bucket of group
// "Gemini 4 Argon", whose JSON fields (the flag, the fraction) are spliced in.
func helperDisabledReply(argonFields string) string {
	return `{"response":{"groups":[
  {"displayName":"Gemini Models","buckets":[
    {"bucketId":"gemini-5h","window":"5h","remainingFraction":0.75,"resetTime":"2126-11-02T19:22:54Z"}]},
  {"displayName":"Gemini 4 Argon","buckets":[
    {"bucketId":"gemini-argon-wtu-weekly","displayName":"Gemini 4 Argon Weekly Limit","window":"weekly",` + argonFields + `"resetTime":"` + helperDisabledReset + `"}]}]}}`
}

func helperDisabledEvidence(t *testing.T) []antigravityExhaustionEvent {
	t.Helper()
	reset, err := time.Parse(time.RFC3339, helperDisabledReset)
	if err != nil {
		t.Fatal(err)
	}
	return []antigravityExhaustionEvent{{resetAtMs: reset.UnixMilli(), fingerprint: "fp-a"}}
}

func helperHasLabel(metrics []cliAgentUsageMetric, label string) bool {
	for _, m := range metrics {
		if m.Label == label {
			return true
		}
	}
	return false
}

const helperArgonWeeklyLabel = "Gemini 4 Argon — Weekly quota"

// A disabled bucket never becomes a row — not with a literal fraction, not with
// an omitted one that exhaustion evidence names — and it is recorded by
// identity, never charted or kept unobserved.
func TestAntigravitySnapshotFromGroups_DisabledBucketIsNeverARow(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		evidence     bool
	}{
		{"valid fraction", `"disabled":true,"remainingFraction":0.4,`, false},
		{"zero fraction", `"disabled":true,"remainingFraction":0,`, false},
		{"omitted fraction with matching evidence", `"disabled":true,`, true},
		{"omitted fraction without evidence", `"disabled":true,`, false},
		{"flag after the fraction", `"remainingFraction":1,"disabled":true,`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var evidence []antigravityExhaustionEvent
			if tc.evidence {
				evidence = helperDisabledEvidence(t)
			}
			snap, shape, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, helperDisabledReply(tc.fields)), time.Now(), evidence)
			if !ok {
				t.Fatal("the enabled reading beside it must keep the reply an observation")
			}
			if len(snap.Buckets) != 1 || snap.Buckets[0].BucketID != "gemini-5h" {
				t.Errorf("buckets=%+v, want only the enabled Gemini reading", snap.Buckets)
			}
			if len(snap.UnobservedBuckets) != 0 {
				t.Errorf("unobserved=%+v, want a disabled bucket never kept unobserved", snap.UnobservedBuckets)
			}
			want := antigravityDisabledBucket{BucketID: "gemini-argon-wtu-weekly", Group: "Gemini 4 Argon",
				DisplayName: "Gemini 4 Argon Weekly Limit", Window: "weekly", ResetTime: helperDisabledReset}
			if len(snap.DisabledBuckets) != 1 || snap.DisabledBuckets[0] != want {
				t.Errorf("disabled=%+v, want %+v", snap.DisabledBuckets, want)
			}
			if shape.Disabled != 1 || shape.OmittedCharted != 0 || shape.OmittedUnobserved != 0 {
				t.Errorf("shape=%s, want one disabled bucket and no omitted one charted or unobserved", shape)
			}
			if metrics := antigravityQuotaMetrics(snap, time.Now()); helperHasLabel(metrics, helperArgonWeeklyLabel) || len(metrics) != 1 {
				t.Errorf("rows=%+v, want only the Gemini 5h row", metrics)
			}
		})
	}
}

// An enabled bucket that is spent keeps today's behaviour: a literal zero, or
// an omitted fraction that evidence names, is a 100%-consumed row.
func TestAntigravitySnapshotFromGroups_EnabledExhaustedBucketIsStillARow(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		evidence     bool
	}{
		{"literal zero", `"remainingFraction":0,`, false},
		{"literal zero, disabled false", `"disabled":false,"remainingFraction":0,`, false},
		{"omitted with matching evidence", ``, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var evidence []antigravityExhaustionEvent
			if tc.evidence {
				evidence = helperDisabledEvidence(t)
			}
			snap, _, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, helperDisabledReply(tc.fields)), time.Now(), evidence)
			if !ok || len(snap.DisabledBuckets) != 0 {
				t.Fatalf("ok=%v disabled=%+v, want an observation with nothing disabled", ok, snap.DisabledBuckets)
			}
			var argon *cliAgentUsageMetric
			metrics := antigravityQuotaMetrics(snap, time.Now())
			for i := range metrics {
				if metrics[i].Label == helperArgonWeeklyLabel {
					argon = &metrics[i]
				}
			}
			if argon == nil || argon.Consumed == nil || *argon.Consumed != 100 || argon.Model != "gemini-4-argon" {
				t.Fatalf("Argon row=%+v, want 100%% consumed and gemini-4-argon's", argon)
			}
		})
	}
}

// Only a literal true disables: every other value of the flag behaves exactly
// like an omitted one, and none of them fails the reply.
func TestAntigravityWireDisabled_OnlyLiteralTrueDisables(t *testing.T) {
	for _, tc := range []struct{ name, fields string }{
		{"absent", ``},
		{"false", `"disabled":false,`},
		{"null", `"disabled":null,`},
		{"string true", `"disabled":"true",`},
		{"number one", `"disabled":1,`},
		{"object", `"disabled":{"value":true},`},
		{"array", `"disabled":[true],`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap, shape, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, helperDisabledReply(tc.fields+`"remainingFraction":0.4,`)), time.Now(), nil)
			if !ok || shape.Disabled != 0 || len(snap.DisabledBuckets) != 0 || len(snap.Buckets) != 2 {
				t.Fatalf("ok=%v shape=%s disabled=%+v buckets=%d, want the Argon bucket charted as enabled",
					ok, shape, snap.DisabledBuckets, len(snap.Buckets))
			}
		})
	}
}

// A mixed group keeps its enabled bucket; a disabled bucket sharing a window
// identity with an enabled one never displaces it; and an unrecognised
// disabled window is counted but neither stored nor an observation.
func TestAntigravitySnapshotFromGroups_MixedAndDuplicateDisabledBuckets(t *testing.T) {
	const reply = `{"response":{"groups":[
  {"displayName":"Gemini Models","buckets":[
    {"bucketId":"gemini-5h","window":"5h","remainingFraction":0.5,"resetTime":"2126-11-02T19:22:54Z","disabled":true},
    {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.25,"resetTime":"2126-11-09T14:22:54Z"},
    {"bucketId":"gemini-7d","window":"7d","remainingFraction":0,"resetTime":"2126-11-09T14:22:54Z","disabled":true},
    {"bucketId":"gemini-fortnight","window":"fortnightly","remainingFraction":0,"resetTime":"2126-11-09T14:22:54Z","disabled":true}]}]}}`
	snap, shape, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, reply), time.Now(), nil)
	if !ok {
		t.Fatal("want an observation")
	}
	if shape.Disabled != 3 || len(snap.DisabledBuckets) != 2 {
		t.Errorf("shape=%s disabled=%+v, want three counted and the two recognised windows kept", shape, snap.DisabledBuckets)
	}
	metrics := antigravityQuotaMetrics(snap, time.Now())
	if len(metrics) != 1 || metrics[0].Label != "Gemini — Weekly quota" || metrics[0].Remaining == nil || *metrics[0].Remaining != 25 {
		t.Fatalf("rows=%+v, want only the enabled weekly row at 25%% left (the disabled 7d must not displace it)", metrics)
	}
	helperSignsAntigravityMetrics(t, metrics)
}

// An all-disabled reply with a recognised window is an observation with no
// rows; one whose disabled buckets all have unrecognised windows is not, and
// must not alter the cache.
func TestAntigravitySnapshotFromGroups_AllDisabledObservationRule(t *testing.T) {
	const recognised = `{"response":{"groups":[{"displayName":"Gemini 4 Argon","buckets":[
    {"bucketId":"argon-weekly","window":"weekly","remainingFraction":0,"resetTime":"2126-11-09T14:22:54Z","disabled":true}]}]}}`
	snap, _, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, recognised), time.Now(), nil)
	if !ok || len(snap.Buckets) != 0 || len(snap.DisabledBuckets) != 1 {
		t.Fatalf("ok=%v snap=%+v, want an observation holding one disabled bucket", ok, snap)
	}
	if metrics := antigravityQuotaMetrics(snap, time.Now()); len(metrics) != 0 {
		t.Errorf("rows=%+v, want none", metrics)
	}

	cache := filepath.Join(t.TempDir(), "agyq.json")
	helperIsolateAntigravityQuotaState(t, cache)
	prior := helperArgonSnapshot(t)
	prior.Account = "ada@example.com"
	prior.AccountFingerprint = fingerprintAccount("antigravity", prior.Account)
	saveAntigravityQuotaSnapshot(prior)
	before, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}

	const unrecognised = `{"response":{"groups":[{"displayName":"Gemini 4 Argon","buckets":[
    {"bucketId":"argon-fortnight","window":"fortnightly","remainingFraction":0.5,"resetTime":"2126-11-09T14:22:54Z","disabled":true}]}]}}`
	later := helperArgonObservedAt.Add(time.Hour)
	bad, shape, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, unrecognised), later, nil)
	if ok || shape.Disabled != 1 {
		t.Fatalf("ok=%v shape=%s, want a counted disabled bucket and no observation", ok, shape)
	}
	bad.Account = prior.Account
	bad.AccountFingerprint = prior.AccountFingerprint
	bad.ObservedAt = later.Format(time.RFC3339)
	if saveAntigravityQuotaSnapshotIfNewer(bad) {
		t.Error("a non-observation replaced the cache")
	}
	after, _ := os.ReadFile(cache)
	if string(after) != string(before) {
		t.Error("the cache changed")
	}
}

// Disabled records share the 32-entry cap after the numeric buckets and ahead
// of the unobserved ones, each clamped and under a recognised window only.
func TestSanitizeAntigravityQuotaSnapshot_DisabledBucketsPrecedeUnobserved(t *testing.T) {
	snap := antigravityQuotaSnapshot{ObservedAt: "2026-11-02T14:22:54Z"}
	for i := 0; i < cliUsageMaxMetricsPerProvider-2; i++ {
		snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{Group: fmt.Sprintf("Group %02d", i), Window: "weekly", RemainingFraction: 0.5})
	}
	snap.DisabledBuckets = []antigravityDisabledBucket{
		{Group: "Gemini 4 Argon", Window: "fortnightly"},
		{Group: "Gemini 4 Argon", DisplayName: strings.Repeat("D", 500), Window: "weekly"},
	}
	snap.UnobservedBuckets = []antigravityUnobservedBucket{{Group: "Gemini 4 Carbon", Window: "weekly"}}
	out := sanitizeAntigravityQuotaSnapshot(snap)
	if len(out.Buckets) != cliUsageMaxMetricsPerProvider-2 || len(out.DisabledBuckets) != 1 || len(out.UnobservedBuckets) != 1 {
		t.Fatalf("buckets=%d disabled=%+v unobserved=%+v, want the numeric buckets, one disabled and one unobserved",
			len(out.Buckets), out.DisabledBuckets, out.UnobservedBuckets)
	}
	if len(out.DisabledBuckets[0].DisplayName) != antigravityQuotaMaxBucketFieldBytes {
		t.Errorf("display name kept %d bytes, want it clamped to %d", len(out.DisabledBuckets[0].DisplayName), antigravityQuotaMaxBucketFieldBytes)
	}

	// With one slot left, the disabled record wins it.
	snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{Group: "Group last", Window: "weekly", RemainingFraction: 0.5})
	out = sanitizeAntigravityQuotaSnapshot(snap)
	if len(out.DisabledBuckets) != 1 || len(out.UnobservedBuckets) != 0 {
		t.Errorf("disabled=%+v unobserved=%+v, want the last slot to the disabled record", out.DisabledBuckets, out.UnobservedBuckets)
	}
}

// A disabled one-model group publishes no row, so routing leaves its model
// unpooled; the remaining rows still sign. Enabled and spent, the same group
// still pools its model.
func TestAnnotateModelPoolsFromUsage_DisabledArgonLeavesTheModelUnpooled(t *testing.T) {
	disabled := strings.Replace(helperArgonReply,
		`"window":"weekly","remainingFraction":0,`, `"window":"weekly","remainingFraction":0,"disabled":true,`, 1)
	if disabled == helperArgonReply {
		t.Fatal("fixture: the Argon bucket was not marked disabled")
	}
	snap, _, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, disabled), helperArgonObservedAt, nil)
	if !ok {
		t.Fatal("want an observation")
	}
	metrics := antigravityQuotaMetrics(snap, helperArgonObservedAt)
	if len(metrics) != 4 || helperHasLabel(metrics, helperArgonWeeklyLabel) {
		t.Fatalf("rows=%+v, want the four family rows and no Argon row", metrics)
	}
	for _, m := range metrics {
		if m.Model != "" {
			t.Errorf("row %q names model %q, want none", m.Label, m.Model)
		}
	}
	helperSignsAntigravityMetrics(t, metrics)
	for _, model := range annotateModelPoolsFromUsage(helperAgyArgonFamilies(t), metrics) {
		if model.Pool != "" {
			t.Errorf("%s pool = %q, want none", model.ID, model.Pool)
		}
	}

	enabled := antigravityQuotaMetrics(helperArgonSnapshot(t), helperArgonObservedAt)
	if argon := helperMetricLabeled(t, enabled, helperArgonWeeklyLabel); argon.Consumed == nil || *argon.Consumed != 100 {
		t.Errorf("enabled Argon row=%+v, want 100%% consumed", argon)
	}
	pooled := false
	for _, model := range annotateModelPoolsFromUsage(helperAgyArgonFamilies(t), enabled) {
		pooled = pooled || (model.ID == "gemini-4-argon" && model.Pool == "gemini-4-argon")
	}
	if !pooled {
		t.Error("an enabled, spent Argon bucket must still pool gemini-4-argon")
	}
}

// A rolled-back agent reads the cache this one wrote and charts every
// `buckets` entry: a disabled bucket must never be one.
func TestAntigravityQuotaCache_OlderAgentNeverChartsADisabledBucket(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "agyq.json")
	helperIsolateAntigravityQuotaState(t, cache)
	disabled := strings.Replace(helperArgonReply,
		`"window":"weekly","remainingFraction":0,`, `"window":"weekly","remainingFraction":0,"disabled":true,`, 1)
	snap, _, _ := antigravitySnapshotFromGroups(helperGroupsJSON(t, disabled), helperArgonObservedAt, nil)
	snap.Account = "ada@example.com"
	snap.AccountFingerprint = fingerprintAccount("antigravity", snap.Account)
	saveAntigravityQuotaSnapshot(snap)

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var older struct {
		Buckets []struct {
			BucketID string `json:"bucketId"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatal(err)
	}
	if len(older.Buckets) != 4 {
		t.Fatalf("an older reader sees %d buckets, want the four enabled ones", len(older.Buckets))
	}
	for _, b := range older.Buckets {
		if b.BucketID == "gemini-argon-wtu-weekly" {
			t.Error("the disabled Argon bucket is in `buckets`")
		}
	}
	replayed, ok := loadAntigravityQuotaSnapshotByProducer()
	if !ok || len(replayed.DisabledBuckets) != 1 || replayed.SchemaVersion != antigravityQuotaSchemaVersion {
		t.Fatalf("replayed=%+v ok=%v, want the disabled record kept under schema %d", replayed, ok, antigravityQuotaSchemaVersion)
	}
	if helperHasLabel(antigravityQuotaMetrics(replayed, helperArgonObservedAt), helperArgonWeeklyLabel) {
		t.Error("the replayed disabled Argon bucket was published")
	}
}

// The gather's own loopback read: a live server answering with every bucket
// disabled replaces the cached enabled rows, and the card shows the two Unknown
// placeholders instead of replaying the older figure.
func TestAntigravityUsageParser_LiveAllDisabledReplyReplacesTheCache(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(t.TempDir(), "agyq.json")
	helperIsolateAntigravityQuotaState(t, cache)
	prior := helperArgonSnapshot(t)
	prior.Account = "ada@example.com"
	prior.AccountFingerprint = fingerprintAccount("antigravity", prior.Account)
	saveAntigravityQuotaSnapshot(prior)

	const reply = `{"response":{"groups":[{"displayName":"Gemini Models","buckets":[
    {"bucketId":"gemini-5h","window":"5h","remainingFraction":0.9,"disabled":true,"resetTime":"2126-08-12T04:37:41Z"},
    {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.75,"disabled":true,"resetTime":"2126-08-14T14:17:13Z"}]}]}}`
	helperAntigravityServer(t, filepath.Join(home, ".gemini", "antigravity-cli"), reply, helperStatusJSON)

	usage := helperGather(t, home)
	if len(usage.Metrics) != 2 {
		t.Fatalf("metrics=%+v, want the two placeholders", usage.Metrics)
	}
	for _, m := range usage.Metrics {
		if !m.Unknown || m.Consumed != nil || m.Model != "" {
			t.Errorf("metric %+v, want an Unknown placeholder", m)
		}
	}
	cached, ok := cachedAntigravityQuotaSnapshot()
	if !ok || len(cached.Buckets) != 0 || len(cached.DisabledBuckets) != 2 {
		t.Errorf("cached=%+v ok=%v, want the all-disabled reading to replace the prior rows", cached, ok)
	}
}
