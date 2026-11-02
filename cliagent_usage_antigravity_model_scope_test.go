package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// 2026-11-02: paid Google AI Pro meters Gemini 4 Argon in a weekly group of its
// own beside "Gemini Models" and "Claude and GPT models". The row went out with
// no model, shared-constants read it as the account's own window, and one
// spent Argon week made the card say "Limit reached" and routing skip every
// Antigravity model while Flash, Claude and GPT had 100% left. These pin the
// device half: a one-model group's rows name that model, a respelled window
// keeps its identity, and no reading carries two rows of one identity.

// helperArgonReply is that reading as the wire carries it (agent.log:
// groups=3 buckets=5, every fraction a literal). The Argon bucket is verbatim
// from the affected computer's cache; the family buckets keep their shape.
const helperArgonReply = `{"response":{"groups":[
  {"displayName":"Gemini Models","buckets":[
    {"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":1,"resetTime":"2026-11-09T14:22:54Z"},
    {"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":1,"resetTime":"2026-11-02T19:22:54Z"}]},
  {"displayName":"Claude and GPT models","buckets":[
    {"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":1,"resetTime":"2026-11-09T14:22:54Z"},
    {"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":1,"resetTime":"2026-11-02T19:22:54Z"}]},
  {"displayName":"Gemini 4 Argon","buckets":[
    {"bucketId":"gemini-argon-wtu-weekly","displayName":"Gemini 4 Argon Weekly Limit","window":"weekly","remainingFraction":0,"resetTime":"2026-11-03T08:06:36Z"}]}]}}`

var helperArgonObservedAt = time.Date(2026, 11, 2, 14, 22, 54, 0, time.UTC)

func helperArgonSnapshot(t *testing.T) antigravityQuotaSnapshot {
	t.Helper()
	snap, _, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, helperArgonReply), helperArgonObservedAt, nil)
	if !ok {
		t.Fatal("the Argon reading must be an observation")
	}
	return snap
}

func helperMetricLabeled(t *testing.T, metrics []cliAgentUsageMetric, label string) cliAgentUsageMetric {
	t.Helper()
	for _, metric := range metrics {
		if metric.Label == label {
			return metric
		}
	}
	t.Fatalf("no row %q in %+v", label, metrics)
	return cliAgentUsageMetric{}
}

func helperSignsAntigravityMetrics(t *testing.T, metrics []cliAgentUsageMetric) {
	t.Helper()
	if _, _, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1, true, []cliAgentUsage{{Provider: "antigravity", Metrics: metrics}}, nil); err != nil {
		t.Fatalf("the rows were rejected from the signed refresh: %v", err)
	}
}

func TestAntigravityQuotaMetrics_ModelScopedGroupCarriesItsKey(t *testing.T) {
	metrics := antigravityQuotaMetrics(helperArgonSnapshot(t), helperArgonObservedAt)
	want := []struct{ label, model string }{
		{"Gemini — 5-hour session window", ""},
		{"Gemini — Weekly quota", ""},
		{"Claude and GPT — 5-hour session window", ""},
		{"Claude and GPT — Weekly quota", ""},
		{"Gemini 4 Argon — Weekly quota", "gemini-4-argon"},
	}
	if len(metrics) != len(want) {
		t.Fatalf("rows=%+v, want %d", metrics, len(want))
	}
	for i, w := range want {
		if metrics[i].Label != w.label || metrics[i].Model != w.model {
			t.Errorf("row %d = %q model %q, want %q model %q", i, metrics[i].Label, metrics[i].Model, w.label, w.model)
		}
	}
	argon := metrics[4]
	if argon.Kind != limitKindWeekly || argon.Unknown || argon.Remaining == nil || *argon.Remaining != 0 ||
		argon.ResetAt != "2026-11-03T08:06:36Z" {
		t.Errorf("Argon row = %+v, want a spent week resetting 2026-11-03T08:06:36Z", argon)
	}
	helperSignsAntigravityMetrics(t, metrics)

	// Once the week rolls over the row is Unknown, and still that model's.
	after := antigravityQuotaMetrics(helperArgonSnapshot(t), time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	if rolled := helperMetricLabeled(t, after, "Gemini 4 Argon — Weekly quota"); !rolled.Unknown || rolled.Model != "gemini-4-argon" {
		t.Errorf("rolled-over Argon row = %+v, want Unknown and still gemini-4-argon's", rolled)
	}
}

// Which groups name a model. shared-constants decides the same from the label
// alone (codingAgentWindowModelScope), so an older agent's label-only row and
// this one are one window there; only the plural "Models" check is the
// device's own, because the label drops that suffix.
func TestAntigravityQuotaMetrics_OnlyAOneModelGroupNamesItsModel(t *testing.T) {
	now := helperArgonObservedAt
	for _, tc := range []struct{ group, model, why string }{
		{"Gemini Models", "", "the Gemini family pool"},
		{"Claude and GPT models", "", "the Claude and GPT family pool"},
		{"claude  and GPT", "", "a family pool whatever its case and spacing"},
		{"Gemini 3 Models", "", "a plural Models group holds several models"},
		{"Weekly", "", "a period"},
		{"5-hour", "", "a period"},
		{"Overall", "", "the whole account"},
		{"All models", "", "the whole account"},
		{"Gemini — Argon", "", "an em dash in the name ends the label's group early"},
		{"日本語", "", "no ASCII letter or digit"},
		{"Gemini 4 Argon", "gemini-4-argon", "agy's own family id"},
		{"Gemini 4 Carbon", "gemini-4-carbon", "the next such group needs no code change"},
		{"Gemini 4 Argon Model", "gemini-4-argon", "a singular Model suffix is dropped as the label drops it"},
		{"Claude Opus 5.5", "claude-opus-5.5", "dots are kept"},
		// Folded as JavaScript folds, so a name is a pool on both sides or neither.
		{"Da\xc4\xb0ly", "da-ly", "a dotted capital I lower-cases to i plus a combining dot, never to daily"},
		{"Claude\xef\xbb\xbfand GPT", "", "U+FEFF is JavaScript whitespace"},
		{"Claude\xc2\x85and GPT", "claude-and-gpt", "U+0085 is not"},
	} {
		snap := antigravityQuotaSnapshot{
			ObservedAt: now.Format(time.RFC3339),
			Buckets:    []antigravityQuotaBucket{{Group: tc.group, Window: "weekly", RemainingFraction: 0.5, ResetTime: "2026-11-09T00:00:00Z"}},
		}
		metrics := antigravityQuotaMetrics(snap, now)
		if len(metrics) != 1 || metrics[0].Model != tc.model {
			t.Errorf("%q: rows=%+v, want model %q (%s)", tc.group, metrics, tc.model, tc.why)
		}
	}
}

// The key must equal shared-constants codingAgentGroupModelKey byte for byte;
// the vector file is a verbatim copy of that repo's fixture.
func TestAntigravityGroupModelKey_SharedVectors(t *testing.T) {
	data, err := os.ReadFile("testdata/antigravity_group_model_key_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Version int `json:"version"`
		Vectors []struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if file.Version != 1 || len(file.Vectors) < 20 {
		t.Fatalf("version=%d vectors=%d, want version 1 and the full set", file.Version, len(file.Vectors))
	}
	for _, v := range file.Vectors {
		if got := antigravityGroupModelKey(v.Name); got != v.Key {
			t.Errorf("antigravityGroupModelKey(%q) = %q, want %q", v.Name, got, v.Key)
		}
	}
}

// Two rows of one identity reject the WHOLE refresh in both receipt
// verifiers, for every provider. They are published once, the more
// constrained: a figure beats Unknown, and the one with less left wins.
func TestAntigravityQuotaMetrics_NeverEmitsADuplicateIdentity(t *testing.T) {
	now := helperArgonObservedAt
	snap := antigravityQuotaSnapshot{
		ObservedAt: now.Format(time.RFC3339),
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini 4 Argon", Window: "weekly", RemainingFraction: 0.4, ResetTime: "2026-11-09T00:00:00Z"},
			{Group: "Gemini 4 Argon", Window: "7d", RemainingFraction: 0.1, ResetTime: "2026-11-09T00:00:00Z"},
			// A window that rolled over, then a figure for the same window.
			{Group: "Gemini Models", Window: "5h", RemainingFraction: 0.2, ResetTime: "2026-11-02T09:00:00Z"},
			{Group: "Gemini Models", Window: "300m", RemainingFraction: 0.7, ResetTime: "2026-11-02T18:00:00Z"},
		},
		UnobservedBuckets: []antigravityUnobservedBucket{
			{Group: "Gemini 4 Argon", Window: "604800s", ResetTime: "2026-11-09T00:00:00Z"},
			{Group: "Gemini Models", Window: "weekly", ResetTime: "2026-11-09T00:00:00Z"},
		},
	}
	metrics := antigravityQuotaMetrics(snap, now)
	seen := map[string]bool{}
	for _, metric := range metrics {
		identity := metric.Kind + "\x00" + metric.Label + "\x00" + metric.Model
		if seen[identity] {
			t.Errorf("duplicate identity %q", identity)
		}
		seen[identity] = true
	}
	if len(metrics) != 2 {
		t.Fatalf("rows=%+v, want two identities", metrics)
	}
	if argon := helperMetricLabeled(t, metrics, "Gemini 4 Argon — Weekly quota"); argon.Unknown || argon.Remaining == nil || *argon.Remaining != 10 {
		t.Errorf("Argon row = %+v, want the lower of the two figures (10)", argon)
	}
	if session := helperMetricLabeled(t, metrics, "Gemini — 5-hour session window"); session.Unknown || session.Remaining == nil || *session.Remaining != 70 {
		t.Errorf("session row = %+v, want the figure over the rolled-over row", session)
	}
	for _, metric := range metrics {
		if metric.Label == "Gemini — Weekly quota" {
			t.Errorf("an unobserved bucket was published: %+v", metric)
		}
	}
	helperSignsAntigravityMetrics(t, metrics)
}

// A live reading is rendered unsanitized and its replay from the cache
// clamped. Both render from the clamped group, so one bucket has one label and
// one model whichever path the card took, and a long group stays inside the
// receipt's 256-byte label bound.
func TestAntigravityQuotaMetrics_FreshAndReplayedRowsMatchForALongGroup(t *testing.T) {
	now := helperArgonObservedAt
	for _, tc := range []struct {
		group     string
		wantModel bool
	}{
		{strings.Repeat("Gemini Argon Preview ", 8) + "Edition", true},
		// The 96-byte cut ends on " Models ", a trailing space the replay's
		// second clamp trims: both must read it as the plural family group.
		{strings.Repeat("a", 88) + " Models tail", false},
	} {
		fresh := antigravityQuotaSnapshot{
			ObservedAt:         now.Format(time.RFC3339),
			AccountFingerprint: fingerprintAccount("antigravity", "ada@example.com"),
			Buckets:            []antigravityQuotaBucket{{Group: tc.group, Window: "weekly", RemainingFraction: 0.25, ResetTime: "2026-11-09T00:00:00Z"}},
			UnobservedBuckets:  []antigravityUnobservedBucket{{Group: tc.group, Window: "5h", ResetTime: "2026-11-02T18:00:00Z"}},
		}
		live := antigravityQuotaMetrics(fresh, now)
		replayed := antigravityQuotaMetrics(sanitizeAntigravityQuotaSnapshot(fresh), now)
		if len(live) != 1 || !reflect.DeepEqual(live, replayed) {
			t.Fatalf("%q: live=%+v\nreplayed=%+v, want the same one row", tc.group, live, replayed)
		}
		for _, metric := range live {
			if len(metric.Label) > 256 || (metric.Model != "") != tc.wantModel || len(metric.Model) > cliUsageMaxModelPoolLength {
				t.Errorf("%q: row %+v, want a bounded label, model present=%v", tc.group, metric, tc.wantModel)
			}
		}
		helperSignsAntigravityMetrics(t, live)
	}
}

func TestAntigravityWindowKind_GenericSpellings(t *testing.T) {
	const (
		session = "5-hour session window"
		daily   = "Daily quota"
		weekly  = "Weekly quota"
		monthly = "Monthly quota"
	)
	for _, tc := range []struct{ window, kind, label string }{
		// The four spellings Google has used, unchanged.
		{"5h", limitKindSession, session},
		{" Daily ", limitKindDaily, daily},
		{"WEEKLY", limitKindWeekly, weekly},
		{"monthly", limitKindMonthly, monthly},
		// The same lengths respelled keep the kind AND the label.
		{"300m", limitKindSession, session},
		{"18000s", limitKindSession, session},
		{"5 hours", limitKindSession, session},
		{"5-hour", limitKindSession, session},
		{"PT5H", limitKindSession, session},
		{"24h", limitKindDaily, daily},
		{"1d", limitKindDaily, daily},
		{"86400s", limitKindDaily, daily},
		{"7d", limitKindWeekly, weekly},
		{"1w", limitKindWeekly, weekly},
		{"168h", limitKindWeekly, weekly},
		{"604800s", limitKindWeekly, weekly},
		{"604800.000s", limitKindWeekly, weekly},
		{"p7d", limitKindWeekly, weekly},
		{"P1W", limitKindWeekly, weekly},
		{"30d", limitKindMonthly, monthly},
		{"1mo", limitKindMonthly, monthly},
		{"P1M", limitKindMonthly, monthly},
		// Seconds need up to eight digits for a month or a year.
		{"2592000s", limitKindMonthly, monthly},
		{"PT2592000S", limitKindMonthly, monthly},
		{"31622400s", "window_366d", "366-day window"},
		{"28 days", limitKindMonthly, monthly},
		// Band edges: only a length inside a band is canonical.
		{"294m", "window_294m", "294-minute window"},
		{"295m", limitKindSession, session},
		{"305m", limitKindSession, session},
		{"306m", "window_306m", "306-minute window"},
		{"1434m", "window_1434m", "1434-minute window"},
		{"1435m", limitKindDaily, daily},
		{"1445m", limitKindDaily, daily},
		{"1446m", "window_1446m", "1446-minute window"},
		{"10019m", "window_10019m", "10019-minute window"},
		{"10020m", limitKindWeekly, weekly},
		{"10140m", limitKindWeekly, weekly},
		{"10141m", "window_10141m", "10141-minute window"},
		{"40319m", "window_40319m", "40319-minute window"},
		{"44640m", limitKindMonthly, monthly},
		{"44641m", "window_44641m", "44641-minute window"},
		// Any other length is a window of its own, named in its largest unit,
		// one identity however it is spelled.
		{"4h", "window_4h", "4-hour window"},
		{"14d", "window_14d", "14-day window"},
		{"2w", "window_14d", "14-day window"},
		{"p14d", "window_14d", "14-day window"},
		{"6d", "window_6d", "6-day window"},
		{"36h", "window_36h", "36-hour window"},
		{"48h", "window_2d", "2-day window"},
		{"pt30m", "window_30m", "30-minute window"},
		{"1.5h", "window_90m", "90-minute window"},
		{"366d", "window_366d", "366-day window"},
	} {
		kind, label, ok := antigravityWindowKind(tc.window)
		if !ok || kind != tc.kind || label != tc.label {
			t.Errorf("antigravityWindowKind(%q) = %q, %q, %v; want %q, %q", tc.window, kind, label, ok, tc.kind, tc.label)
		}
		if kind == "hint" || len(kind) > 32 {
			t.Errorf("antigravityWindowKind(%q) kind %q, want a bounded kind that is never a hint", tc.window, kind)
		}
	}
	// A word, a bare number, a compound, a year or less than a minute is not
	// one length: skipped exactly as an unknown window always was.
	for _, window := range []string{"", "fortnightly", "hint", "hourly", "7", "0d", "30s", "367d", "31622460s", "999999999s", "1y", "p1y",
		"p1dt12h", "p", "pt", "7 dayz", "window_4h", "1000000d"} {
		if kind, label, ok := antigravityWindowKind(window); ok {
			t.Errorf("antigravityWindowKind(%q) = %q, %q; want it skipped", window, kind, label)
		}
	}
}

// A window Google respells keeps the row it always was: same kind, label and
// model, so its history and its cross-device identity carry on.
func TestAntigravityQuotaMetrics_RespelledWindowKeepsItsIdentity(t *testing.T) {
	now := helperArgonObservedAt
	row := func(group, window string) cliAgentUsageMetric {
		t.Helper()
		snap := antigravityQuotaSnapshot{
			ObservedAt: now.Format(time.RFC3339),
			Buckets:    []antigravityQuotaBucket{{Group: group, Window: window, RemainingFraction: 0, ResetTime: "2026-11-03T08:06:36Z"}},
		}
		metrics := antigravityQuotaMetrics(snap, now)
		if len(metrics) != 1 {
			t.Fatalf("%s %s: rows=%+v", group, window, metrics)
		}
		return metrics[0]
	}
	for _, group := range []string{"Gemini 4 Argon", "Gemini Models"} {
		canonical := row(group, "weekly")
		for _, respelled := range []string{"7d", "604800s", "P7D", "1 week"} {
			if got := row(group, respelled); !reflect.DeepEqual(got, canonical) {
				t.Errorf("%s %q = %+v, want the weekly row %+v", group, respelled, got, canonical)
			}
		}
	}
}

// The existing fortnightly tests stand: a word is still skipped. The line they
// draw now runs between a word and a length.
func TestAntigravityQuotaMetrics_OpenLengthIsPlottedUnderItsOwnKind(t *testing.T) {
	now := helperArgonObservedAt
	snap := antigravityQuotaSnapshot{
		ObservedAt: now.Format(time.RFC3339),
		Buckets: []antigravityQuotaBucket{
			{Group: "Gemini Models", Window: "fortnightly", RemainingFraction: 0.5, ResetTime: "2026-11-09T00:00:00Z"},
			{Group: "Gemini Models", Window: "14d", RemainingFraction: 0.5, ResetTime: "2026-11-09T00:00:00Z"},
		},
	}
	metrics := antigravityQuotaMetrics(snap, now)
	if len(metrics) != 1 || metrics[0].Kind != "window_14d" || metrics[0].Label != "Gemini — 14-day window" ||
		metrics[0].Remaining == nil || *metrics[0].Remaining != 50 {
		t.Fatalf("rows=%+v, want only the 14-day window, under its own kind", metrics)
	}
}

// An omitted fraction beside a reading: the reply is an observation, and the
// omitted bucket is kept in the snapshot for diagnosis, never published.
// Without a reset, or under an unrecognised window, it is dropped as before.
func TestAntigravitySnapshotFromGroups_OmittedBesideAReadingIsKeptUnobserved(t *testing.T) {
	raw := `{"response":{"groups":[
	  {"displayName":"Gemini Models","buckets":[
	    {"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.6,"resetTime":"2026-11-09T14:22:54Z"},
	    {"bucketId":"gemini-5h","window":"5h"},
	    {"bucketId":"gemini-fortnight","window":"fortnightly","resetTime":"2026-11-09T14:22:54Z"}]},
	  {"displayName":"Gemini 4 Argon","buckets":[
	    {"bucketId":"gemini-argon-wtu-weekly","displayName":"Gemini 4 Argon Weekly Limit","window":"weekly","resetTime":"2026-11-03T08:06:36Z"}]}]}}`
	snap, shape, ok := antigravitySnapshotFromGroups(helperGroupsJSON(t, raw), helperArgonObservedAt, nil)
	if !ok || len(snap.Buckets) != 1 {
		t.Fatalf("snap=%+v ok=%v, want the weekly reading", snap, ok)
	}
	want := []antigravityUnobservedBucket{{BucketID: "gemini-argon-wtu-weekly", Group: "Gemini 4 Argon",
		DisplayName: "Gemini 4 Argon Weekly Limit", Window: "weekly", ResetTime: "2026-11-03T08:06:36Z"}}
	if !reflect.DeepEqual(snap.UnobservedBuckets, want) {
		t.Errorf("unobserved=%+v, want only the Argon bucket", snap.UnobservedBuckets)
	}
	if shape.FractionOmitted != 3 || shape.OmittedUnobserved != 1 || shape.OmittedCharted != 0 {
		t.Errorf("shape=%+v, want three omitted, one kept unobserved", shape)
	}

	metrics := antigravityQuotaMetrics(snap, helperArgonObservedAt)
	if len(metrics) != 1 || metrics[0].Label != "Gemini — Weekly quota" {
		t.Errorf("rows=%+v, want only the Gemini reading: the omitted Argon bucket is never a row", metrics)
	}
	helperSignsAntigravityMetrics(t, metrics)
}

// The cap is shared: numeric buckets first, then unobserved ones while room
// is left, each clamped and only under a recognised window.
func TestSanitizeAntigravityQuotaSnapshot_UnobservedBucketsShareTheCap(t *testing.T) {
	snap := antigravityQuotaSnapshot{ObservedAt: "2026-11-02T14:22:54Z"}
	for i := 0; i < cliUsageMaxMetricsPerProvider-2; i++ {
		snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{Group: fmt.Sprintf("Group %02d", i), Window: "weekly", RemainingFraction: 0.5})
	}
	snap.UnobservedBuckets = []antigravityUnobservedBucket{
		{Group: "Gemini 4 Argon", Window: "fortnightly", ResetTime: "2026-11-03T08:06:36Z"},
		{Group: strings.Repeat("A", 500), Window: "weekly", ResetTime: "2026-11-03T08:06:36Z"},
		{Group: "Gemini 4 Argon", Window: "7d", ResetTime: "2026-11-03T08:06:36Z"},
		{Group: "Gemini 4 Carbon", Window: "weekly", ResetTime: "2026-11-03T08:06:36Z"},
	}
	out := sanitizeAntigravityQuotaSnapshot(snap)
	if len(out.Buckets) != cliUsageMaxMetricsPerProvider-2 || len(out.UnobservedBuckets) != 2 {
		t.Fatalf("buckets=%d unobserved=%+v, want every numeric bucket and two unobserved", len(out.Buckets), out.UnobservedBuckets)
	}
	if len(out.UnobservedBuckets[0].Group) != antigravityQuotaMaxBucketFieldBytes || out.UnobservedBuckets[1].Window != "7d" {
		t.Errorf("unobserved=%+v, want the long group clamped and the fortnightly bucket dropped", out.UnobservedBuckets)
	}
}

// Unobserved buckets never take a row, so they can never push a reading out
// of the signed refresh's cap.
func TestAntigravityQuotaMetrics_UnobservedBucketsNeverTakeARow(t *testing.T) {
	now := helperArgonObservedAt
	snap := antigravityQuotaSnapshot{ObservedAt: now.Format(time.RFC3339)}
	const groups = 20
	for i := 0; i < groups; i++ {
		group := fmt.Sprintf("Group %02d Models", i)
		snap.Buckets = append(snap.Buckets, antigravityQuotaBucket{Group: group, Window: "5h", RemainingFraction: 0.5, ResetTime: "2026-11-02T18:00:00Z"})
		snap.UnobservedBuckets = append(snap.UnobservedBuckets, antigravityUnobservedBucket{Group: group, Window: "weekly", ResetTime: "2026-11-09T00:00:00Z"})
	}
	metrics := antigravityQuotaMetrics(snap, now)
	if len(metrics) != groups {
		t.Fatalf("rows=%d, want the %d readings only", len(metrics), groups)
	}
	for i, metric := range metrics {
		if want := fmt.Sprintf("Group %02d — 5-hour session window", i); metric.Label != want || metric.Unknown {
			t.Errorf("row %d = %q unknown=%v, want the reading %q", i, metric.Label, metric.Unknown, want)
		}
	}
	helperSignsAntigravityMetrics(t, metrics)
}

// A rolled-back agent reads the cache this one wrote. It charts every
// `buckets` entry by its fraction, so an unobserved bucket must never be one:
// decoded with the older reader's shape, the cache holds only the reading.
func TestAntigravityQuotaCache_OlderAgentNeverChartsAnUnobservedBucket(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "agyq.json")
	helperIsolateAntigravityQuotaState(t, cache)
	snap := helperArgonSnapshot(t)
	snap.Buckets = snap.Buckets[:1] // the Gemini week
	snap.UnobservedBuckets = []antigravityUnobservedBucket{{BucketID: "gemini-argon-wtu-weekly", Group: "Gemini 4 Argon",
		DisplayName: "Gemini 4 Argon Weekly Limit", Window: "weekly", ResetTime: "2026-11-03T08:06:36Z"}}
	snap.AccountFingerprint = fingerprintAccount("antigravity", "ada@example.com")
	snap.Account = "ada@example.com"
	saveAntigravityQuotaSnapshot(snap)

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("the reading was not cached: %v", err)
	}
	var older struct {
		Buckets []struct {
			BucketID          string  `json:"bucketId"`
			Group             string  `json:"group"`
			Window            string  `json:"window"`
			RemainingFraction float64 `json:"remainingFraction"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatal(err)
	}
	if len(older.Buckets) != 1 || older.Buckets[0].BucketID != "gemini-weekly" || older.Buckets[0].RemainingFraction != 1 {
		t.Fatalf("an older reader sees buckets %+v, want only the Gemini reading", older.Buckets)
	}

	replayed, ok := loadAntigravityQuotaSnapshotByProducer()
	if !ok || len(replayed.UnobservedBuckets) != 1 {
		t.Fatalf("replayed=%+v ok=%v, want the unobserved bucket kept", replayed, ok)
	}
	for _, metric := range antigravityQuotaMetrics(replayed, helperArgonObservedAt) {
		if metric.Label == "Gemini 4 Argon — Weekly quota" {
			t.Errorf("the replayed unobserved Argon bucket was published: %+v", metric)
		}
	}
}
