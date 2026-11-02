// cliagent_usage_claudecode_windowset.go — the set of quota windows Claude
// REPORTS for the signed-in account, and the rows that set renders as.
//
// Why this exists:
//
//	Anthropic renamed its plans on 2026-10-30 (Max 20x -> Ultra, Max 5x -> Max),
//	and a verified Ultra account has NO five-hour window: only a weekly
//	all-models limit and a separate weekly Fable limit. The card nonetheless
//	kept a "5-hour session window" row, because the rows were a fixed layout of
//	three (session / weekly / Fable) and nothing ever told the cache a window was
//	gone: the probe only writes the windows a reply supplies, a bucket stamped
//	without a reset never rolls over, and a claude.ai login's empty fingerprint
//	never prunes. A Max-era five_hour reading (0 %, no reset) therefore stayed on
//	the card as a live row — and made every consumer read the account's
//	capacity as UNKNOWN. Any window the code had not modelled (a new kind, a new
//	scoped model, a surface scope) was dropped just as silently.
//
//	The usage endpoint's `limits[]` is, in Claude Code's own schema (2.1.316),
//	"The server's usage rows ... as sent: which meters apply, their scope,
//	labels, severity and order are the server's, so a client renders them
//	verbatim and a new meter needs no client release. Empty when the server
//	reported no meters; null when the body carried no rows at all (a server that
//	predates them)." So the list is authoritative about WHICH windows the
//	account has, and this file is the one place that reads it that way.
//
// The rules, in the order they bite:
//
//  1. Report what the provider reports. The window set is the provider's list,
//     not a fixed layout, and a window this agent never modelled becomes a row
//     of its own instead of being dropped.
//  2. Absence is believed only when authoritative (claudeUsageReplyWindows). A
//     failed, partial or legacy-only read never removes a row.
//  3. Newer evidence wins. A reading observed after the last authoritative
//     reply — a stream event, a status-line render — shows its window again
//     (claudeCodeMetricsFromWindowSet).
//  4. Identity stability. The three windows that were always modelled keep
//     their exact kind / label / model ("5-hour session window" / session,
//     "Weekly quota" / weekly, "Weekly Fable" / weekly / fable): the metric
//     identity everywhere downstream is kind+label+model, and two agent
//     versions on one account must keep merging. Only windows that were never
//     modelled get derived labels.
//  5. No wire-contract change. terminal-service's signed receipt rejects any
//     metric field it does not know (METRIC_FIELDS) for the WHOLE refresh, so a
//     generic window travels as free-text kind / label / model within the
//     receipt's bounds: at most 32 rows, strings of at most 256 bytes, and
//     kind+label+model unique per provider.
//  6. Fail safe for routing. A model-scoped window always names its model (a
//     nested sub-limit: spent, it blocks that model, not the account). A window
//     that cannot be tied to a model — a surface scope, an unknown one — names
//     none and is a shared constraint: over-block, never over-admit. Nothing
//     here ever emits kind "hint", which shared-constants reserves.
//
// The set is NOT a prune. Buckets the set no longer names stay in the cache —
// another channel's cache or an import would only resurrect them — and are
// hidden when rows are built, which also means an older agent binary sharing
// the cache (it re-marshals the snapshot and drops the set) simply goes back to
// today's three rows.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// claudeGenericWindowPrefix marks the cache id of a window this agent never
// modelled: `limit:<kind>` plus `:model:<name>` or `:surface:<name>` when the
// entry is scoped (claudeGenericWindow). The prefix keeps those ids out of
// every rule written for the known ones — claudeFableWindowIDs in particular,
// whose tolerant "*fable*" tail would otherwise read a daily Fable-scoped
// window as the weekly Fable row.
//
// An agent binary from before this prefix existed has no such exclusion, so
// where it reads a cache this one writes — a dual-channel computer whose
// status-line hook pins the newer channel's cache, or a rollback — it counts a
// Fable-named generic id (`limit:daily_scoped:model:fable`) as a candidate for
// its "Weekly Fable" row. Accepted as a mixed-version degrade rather than
// spelling the model out of the id: that binary takes the freshest candidate,
// canonical first on a tie, and one probe reply stamps every window it reads
// with the same instant, so the generic id wins only when the account's weekly
// Fable window is absent from (or unreadable in) the newest reply — and then
// the row shows that model's own meter under the weekly label until the binary
// updates. No window the provider reports today has this shape.
const claudeGenericWindowPrefix = "limit:"

const (
	// claudeUsageProbeMaxLimits bounds the limits[] entries one reply may
	// contribute. Claude Code's own schema caps the list at 64 rows; a reply
	// longer than that is outside the contract, so it is read only up to the cap
	// and is NOT authoritative — a set this agent could not hold completely must
	// not hide the windows it left out.
	claudeUsageProbeMaxLimits = 64
	// claudeWindowSlugMaxLen bounds each fragment of a generic window id. The id
	// lives only in the cache (never on the wire), so the bound exists to keep a
	// pathological vendor string from bloating the file, not for the receipt.
	claudeWindowSlugMaxLen = 48
	// claudeWindowMetricKindMaxLen bounds a generic row's metric kind. The
	// CLI_AGENT_LIMIT_KIND vocabulary is open, but a kind is a short code, and a
	// longer one is a sign of a vendor string that is not one.
	claudeWindowMetricKindMaxLen = 32
	// claudeWindowLabelMaxRunes / claudeWindowLabelMaxBytes bound a generic
	// row's label: 64 runes reads on the card; 256 bytes is the receipt's bound
	// on every metric string, and an over-long label fails the WHOLE refresh for
	// every provider (canonicalProvider), not just this row.
	claudeWindowLabelMaxRunes = 64
	claudeWindowLabelMaxBytes = 256
	// claudeWindowSetSkew is how far in the FUTURE Claude Code's own cached reply
	// may claim to have been fetched before it is ignored. That stamp comes from
	// another program's clock and is compared against this agent's; one stamped
	// days ahead would otherwise outrank every probe until the clock caught up.
	claudeWindowSetSkew = 10 * time.Minute
)

// Labels of the three windows that were always modelled. Their identity is
// frozen (rule 4), so a derived label may never take one of them for a
// different window: cross-device merges key rows by kind+label+model, and a
// generic "Weekly quota" from this computer would be merged with another
// computer's seven_day row as if they were the same window.
const (
	claudeSessionLabel = "5-hour session window"
	claudeWeeklyLabel  = "Weekly quota"
	claudeFableLabel   = "Weekly Fable"
)

// claudeReportedWindow is one window an authoritative reply named, as the
// snapshot persists it. A known window (five_hour, seven_day, seven_day_fable,
// …) carries only its id: its row keeps the identity it always had. A window
// this agent never modelled also carries the row it renders as — metric kind,
// label and nested-pool model — derived once from the reply, because limits[]
// carries no label of its own and a later read has nothing but the cache to
// derive it from.
//
// Order is the entry's position in the reply. Generic rows render in the
// server's order (the schema: "rows render grouped under [group], in the
// server's order"), and a persisted order is what makes that deterministic: a
// Go map has no order, and terminal-service's delta-skip hash sorts object keys
// but NOT arrays, so rows built by iterating a map would churn a Firestore
// write plus a history document on every refresh.
//
// ID is the map key the snapshot persists the window under
// (claudeRateLimitSnapshot.ReportedWindows), like a bucket's: a known window
// is then stored as numbers only, and the cache keeps its rule that what it
// holds is metrics, timestamps, fingerprints and closed vocabulary — free text
// appears only in a generic window's row, which the card publishes anyway.
//
// ReadAccount / ReadOrg are set only on a READING's row
// (claudeRateLimitBucket.Row): the login the reading was taken under
// (claudeAccountBinding), so a reading from another login is never shown as
// this one's. A set records its login once, for all its windows, and leaves
// them empty.
type claudeReportedWindow struct {
	ID          string `json:"-"`
	Kind        string `json:"kind,omitempty"`
	Label       string `json:"label,omitempty"`
	Model       string `json:"model,omitempty"`
	Order       int    `json:"order"`
	ReadAccount string `json:"account,omitempty"`
	ReadOrg     string `json:"org,omitempty"`
}

// readBinding is the login a reading's row was taken under.
func (w claudeReportedWindow) readBinding() claudeAccountBinding {
	return claudeAccountBinding{account: w.ReadAccount, org: w.ReadOrg}
}

// claudeWindowSet is one authoritative statement of which windows the account
// has, at the instant it was observed. Two sources produce one: the probe
// (persisted with its buckets, claudeRateLimitSnapshot.ReportedWindows) and
// Claude Code's own cached copy of the same reply in ~/.claude.json
// (claudeDotJSONFacts.usageWindows, re-read every gather, never persisted).
// claudeEffectiveWindowSet picks between them.
type claudeWindowSet struct {
	AtMs    int64
	Windows []claudeReportedWindow
	// Account / Org are the one-way digests (claudeAccountRef, claudeOrgRef) of
	// the Claude account and organization the set was read under, "" when not
	// known. See claudeEffectiveWindowSet for the binding they enforce.
	Account string
	Org     string
}

// binding is the login the set was read under.
func (s *claudeWindowSet) binding() claudeAccountBinding {
	return claudeAccountBinding{account: s.Account, org: s.Org}
}

// claudeSnapshotWindowSet is the probe's persisted set, or nil when the
// snapshot carries none — a cache written before this field existed, by an
// older binary that dropped it, or by a probe that never had an authoritative
// reply. A known EMPTY set (`limits: []`) is still a set: its instant is what
// records it, which is why presence is decided by ReportedWindowsAtMs and not
// by the (omitempty) map. The windows come back in the server's order, ties by
// id — never in map order.
func claudeSnapshotWindowSet(snap claudeRateLimitSnapshot) *claudeWindowSet {
	if snap.ReportedWindowsAtMs <= 0 {
		return nil
	}
	windows := make([]claudeReportedWindow, 0, len(snap.ReportedWindows))
	for id, w := range snap.ReportedWindows {
		w.ID = id
		windows = append(windows, w)
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Order != windows[j].Order {
			return windows[i].Order < windows[j].Order
		}
		return windows[i].ID < windows[j].ID
	})
	return &claudeWindowSet{AtMs: snap.ReportedWindowsAtMs, Windows: windows,
		Account: snap.ReportedAccount, Org: snap.ReportedOrg}
}

// persisted is the set's windows as the snapshot stores them, keyed by id (nil
// for an empty set, which the snapshot records by its instant alone).
func (s *claudeWindowSet) persisted() map[string]claudeReportedWindow {
	if s == nil || len(s.Windows) == 0 {
		return nil
	}
	out := make(map[string]claudeReportedWindow, len(s.Windows))
	for _, w := range s.Windows {
		out[w.ID] = w
	}
	return out
}

// claudeAccountRef is the one-way digest the snapshot records a set's account
// under. Equality is all the binding needs, and the cache file can reach a
// diagnostics upload, so it never carries the account id itself. "" for "".
func claudeAccountRef(accountUUID string) string {
	accountUUID = strings.TrimSpace(accountUUID)
	if accountUUID == "" {
		return ""
	}
	return fingerprintAccount(claudeUsageProvider+":accountUuid", accountUUID)
}

// claudeOrgRef is claudeAccountRef for oauthAccount.organizationUuid.
func claudeOrgRef(orgUUID string) string {
	orgUUID = strings.TrimSpace(orgUUID)
	if orgUUID == "" {
		return ""
	}
	return fingerprintAccount(claudeUsageProvider+":organizationUuid", orgUUID)
}

// claudeAccountBinding is the login a window set, or a reading of a window this
// agent never modelled, describes: the digests of ~/.claude.json's
// oauthAccount.accountUuid and organizationUuid as they were when it was read,
// "" for a part that was not known.
//
// A claude.ai credential carries no account, so the cache fingerprint cannot
// see a `/login`: the previous login's set and readings stay in the cache, and
// these digests are the only evidence they belong to someone else. Both parts,
// because quotas belong to the ORGANIZATION — one Claude user can sign in to
// several (a personal Ultra org, a company Team org) — and Claude Code itself
// treats a change of either as an account switch: it clears its own cached
// usage reply on every login. Each part is compared only when both sides know
// it, so a field one side lacks never reads as a switch.
type claudeAccountBinding struct {
	account string
	org     string
}

// conflicts reports whether two logins are known to differ.
func (b claudeAccountBinding) conflicts(other claudeAccountBinding) bool {
	differ := func(x, y string) bool { return x != "" && y != "" && x != y }
	return differ(b.account, other.account) || differ(b.org, other.org)
}

// binding is the login ~/.claude.json names.
func (f claudeDotJSONFacts) binding() claudeAccountBinding {
	return claudeAccountBinding{account: claudeAccountRef(f.accountUUID), org: claudeOrgRef(f.orgUUID)}
}

// claudeBindGenericReadings stamps every generic reading in `updates` with the
// login it was read under — the reading's own copy of its row, so the reply's
// window set (which records its login once, for the whole set) is untouched.
func claudeBindGenericReadings(updates map[string]claudeRateLimitBucket, login claudeAccountBinding) {
	for id, b := range updates {
		if b.Row == nil {
			continue
		}
		row := *b.Row
		row.ReadAccount, row.ReadOrg = login.account, login.org
		b.Row = &row
		updates[id] = b
	}
}

// claudeBucketsForLogin is the cache's readings minus every reading of a
// window this agent never modelled that was taken under ANOTHER login than
// `current` (claudeAccountBinding). The modelled windows are left alone: the
// next reading of the same window overwrites the previous login's, which a
// generic window's reading — under an id the new login may never report — never
// is. Hidden, not deleted: signing back in to that login shows them again. The
// map is copied only when something is dropped.
func claudeBucketsForLogin(buckets map[string]claudeRateLimitBucket, current claudeAccountBinding) map[string]claudeRateLimitBucket {
	var out map[string]claudeRateLimitBucket
	for id, b := range buckets {
		if b.Row == nil || !b.Row.readBinding().conflicts(current) {
			continue
		}
		if out == nil {
			out = make(map[string]claudeRateLimitBucket, len(buckets))
			for k, v := range buckets {
				out[k] = v
			}
		}
		delete(out, id)
	}
	if out == nil {
		return buckets
	}
	return out
}

/* ─────────────────────────── decoding helpers ─────────────────────────── */

// claudeLooseString decodes a JSON string and reads every other value — null,
// a number, an object — as "", never failing the enclosing decode. For the
// fields this agent reads only to name or group something: a vendor changing
// one of their types must cost that one field, not the whole usage reply (or,
// in ~/.claude.json, the account label sitting beside it).
type claudeLooseString string

func (s *claudeLooseString) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) != nil {
		str = ""
	}
	*s = claudeLooseString(str)
	return nil
}

// claudeUsageProbeScope is a limits[] entry's `scope`, decoded into the parts a
// window is identified by — the model it meters (id and display name) and the
// surface it applies to — instead of the one flattened label it used to be. The
// two route differently (rule 6): a spent model window blocks that model, while
// a surface window cannot be tied to one and is a shared constraint.
//
// The live shape is `{"model":{"id":null,"display_name":"Fable"},"surface":null}`.
// The older shapes claudeUsageProbeLabel accepts keep their meaning: a bare
// string, and a model-less object naming itself (`{"display_name":"Sonnet"}`),
// are the model's name.
//
// Same never-error contract as claudeUsageProbeLabel: a shape we cannot read
// costs this entry only. `malformed` records it — a scope present but neither
// null, a string nor an object, or a model / surface part that is none of those
// — so claudeUsageReplyWindows can drop the entry AND treat the reply's window
// set as partial. Guessing the entry onto another window would misreport it;
// leaving it out of an authoritative set would hide whichever window it really
// is.
type claudeUsageProbeScope struct {
	modelID     string
	modelName   string
	surfaceName string
	malformed   bool
}

func (s *claudeUsageProbeScope) UnmarshalJSON(b []byte) error {
	*s = claudeUsageProbeScope{}
	trimmed := strings.TrimSpace(string(b))
	switch {
	case trimmed == "" || trimmed == "null":
		return nil
	case trimmed[0] == '"':
		var str string
		_ = json.Unmarshal(b, &str)
		s.modelName = str
		return nil
	case trimmed[0] != '{':
		s.malformed = true
		return nil
	}
	var obj struct {
		Model       json.RawMessage   `json:"model"`
		Surface     json.RawMessage   `json:"surface"`
		DisplayName claudeLooseString `json:"display_name"`
		Name        claudeLooseString `json:"name"`
		ID          claudeLooseString `json:"id"`
	}
	if json.Unmarshal(b, &obj) != nil {
		s.malformed = true
		return nil
	}
	modelID, modelName, ok := claudeUsageProbeScopePart(obj.Model)
	if !ok {
		s.malformed = true
		return nil
	}
	surfaceID, surfaceName, ok := claudeUsageProbeScopePart(obj.Surface)
	if !ok {
		s.malformed = true
		return nil
	}
	s.modelID, s.modelName = modelID, modelName
	s.surfaceName = firstNonEmpty(surfaceName, surfaceID)
	if s.modelID == "" && s.modelName == "" && s.surfaceName == "" {
		s.modelName = firstNonEmpty(string(obj.DisplayName), string(obj.Name))
		s.modelID = string(obj.ID)
	}
	return nil
}

// claudeUsageProbeScopePart reads one part of a scope: absent or null (none), a
// string (its name), or an object carrying id / display_name / name. ok is
// false for any other shape. A part whose fields have unexpected types reads as
// nameless rather than malformed: the part is still what it says it is.
func claudeUsageProbeScopePart(raw json.RawMessage) (id, name string, ok bool) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "" || trimmed == "null":
		return "", "", true
	case trimmed[0] == '"':
		var str string
		_ = json.Unmarshal(raw, &str)
		return "", str, true
	case trimmed[0] != '{':
		return "", "", false
	}
	var part struct {
		ID          claudeLooseString `json:"id"`
		DisplayName claudeLooseString `json:"display_name"`
		Name        claudeLooseString `json:"name"`
	}
	if json.Unmarshal(raw, &part) != nil {
		return "", "", false
	}
	return string(part.ID), firstNonEmpty(string(part.DisplayName), string(part.Name)), true
}

/* ─────────────────────────── entry -> window ─────────────────────────── */

// reportedWindow maps a limits[] entry onto the window it reports: the cache id
// and, for a window this agent never modelled, the row it renders as. ok is
// false for a MALFORMED entry — no kind, or a scope we could not read — which
// is dropped (claudeUsageReplyWindows says what that does to the reply).
//
//	session                                    -> five_hour        (unchanged)
//	weekly_all                                 -> seven_day        (unchanged)
//	weekly_scoped naming opus / sonnet / fable -> seven_day_<model> (unchanged)
//	anything else                              -> limit:<kind>[:model:<name>|:surface:<name>]
//
// The three modelled scoped windows keep their substring match on purpose: it
// is what every shipped agent writes, and an Opus or Sonnet window renamed to a
// generic id would split the weekly aggregate between two agent versions on
// one account (rule 4).
func (l claudeUsageProbeLimit) reportedWindow() (claudeReportedWindow, bool) {
	kind := strings.ToLower(strings.TrimSpace(firstNonEmpty(l.Kind, l.Type)))
	switch kind {
	case "":
		return claudeReportedWindow{}, false
	case "session":
		return claudeReportedWindow{ID: claudeWindowFiveHour}, true
	case "weekly_all":
		return claudeReportedWindow{ID: claudeWindowSevenDay}, true
	}
	// Past the two kinds whose window the scope never decided, an unreadable
	// scope leaves the window unknown.
	if l.Scope.malformed {
		return claudeReportedWindow{}, false
	}
	// The model name a window is matched and labelled by: the scope's, then the
	// top-level `model` an older revision of the payload carried.
	modelName := firstNonEmpty(strings.TrimSpace(l.Scope.modelName), strings.TrimSpace(l.Model.Label))
	modelID := strings.TrimSpace(l.Scope.modelID)
	if kind == "weekly_scoped" {
		match := strings.ToLower(firstNonEmpty(modelName, modelID))
		switch {
		case strings.Contains(match, "opus"):
			return claudeReportedWindow{ID: claudeWindowSevenDayOpus}, true
		case strings.Contains(match, "sonnet"):
			return claudeReportedWindow{ID: claudeWindowSevenDaySonnet}, true
		case strings.Contains(match, "fable"):
			return claudeReportedWindow{ID: claudeWindowSevenDayFable}, true
		}
	}
	return claudeGenericWindow(kind, string(l.Group), modelID, modelName, strings.TrimSpace(l.Scope.surfaceName)), true
}

// claudeGenericWindow derives the id and the row of a window this agent never
// modelled. Everything it emits is bounded and printable, because the label and
// model travel to the signed receipt verbatim, and is a pure function of the
// entry, so the same reply always derives the same row.
func claudeGenericWindow(kind, group, modelID, modelName, surfaceName string) claudeReportedWindow {
	kindSlug := claudeWindowSlug(kind)
	groupSlug := claudeWindowSlug(group)
	metricKind, period := claudeLimitMetricKind(groupSlug, kindSlug)
	periodWord := claudeLimitPeriodWord(firstNonEmpty(groupSlug, period))

	w := claudeReportedWindow{ID: claudeGenericWindowPrefix + kindSlug, Kind: metricKind}
	switch {
	case modelID != "" || modelName != "":
		// The id follows the display name, as the label and the model key do —
		// the id only when there is no name. Every reply seen so far sends
		// `id: null`; slugging the id first would move the window to a new cache
		// id the day the provider fills it in, while its row's kind + label +
		// model stayed the same, and the reading under the old id could then
		// stand in for the new one (claudeMetricRows.add).
		w.ID += ":model:" + claudeWindowSlug(firstNonEmpty(modelName, modelID))
		w.Model = claudeWindowModelKey(modelID, modelName)
		w.Label = claudeScopedWindowLabel(periodWord, kindSlug, firstNonEmpty(modelName, modelID))
	case surfaceName != "":
		// No model: a surface window cannot be tied to one, so it is a shared
		// constraint (rule 6) and carries no model at all.
		w.ID += ":surface:" + claudeWindowSlug(surfaceName)
		w.Label = claudeScopedWindowLabel(periodWord, kindSlug, surfaceName)
	default:
		w.Label = claudeUnscopedWindowLabel(periodWord, groupSlug, kindSlug)
	}
	// Never one of the frozen labels (see claudeSessionLabel). " limit" rather
	// than a parenthesised kind: the Coding capacity dialog reads every word
	// after the first as the window's scope unless they are all generic words,
	// and "limit" is one, so "Weekly quota limit" still reads as all models and
	// "Weekly Fable limit" as the Fable scope.
	if claudeIsFrozenLabel(w.Label) {
		w.Label = claudeClampLabel(w.Label + " limit")
	}
	return w
}

// claudeIsFrozenLabel reports whether a label is one of the three the modelled
// windows own, compared case-insensitively — the dialog and the merges compare
// labels as words, and "Weekly fable" from a generic window would be mistaken
// for the Fable row just as surely as an exact copy.
func claudeIsFrozenLabel(label string) bool {
	for _, frozen := range []string{claudeSessionLabel, claudeWeeklyLabel, claudeFableLabel} {
		if strings.EqualFold(strings.TrimSpace(label), frozen) {
			return true
		}
	}
	return false
}

// claudeLimitPeriods are the window-period codes Claude's kinds and groups are
// spelled with, and the CLI_AGENT_LIMIT_KIND each one means. The frontend
// derives a window's length from the kind (session = 5 h, daily = 24 h,
// weekly = 7 d, monthly), so mapping the period is what lets a new meter age
// correctly on the card instead of going stale after half an hour.
var claudeLimitPeriods = []struct{ code, kind string }{
	{"session", limitKindSession},
	{"five_hour", limitKindSession},
	{"weekly", limitKindWeekly},
	{"seven_day", limitKindWeekly},
	{"daily", limitKindDaily},
	{"monthly", limitKindMonthly},
}

// claudeLimitPeriodOf is the period a code begins with — the kind it means and
// the prefix that matched — or ("", "") when it names none.
func claudeLimitPeriodOf(code string) (kind, prefix string) {
	for _, p := range claudeLimitPeriods {
		if strings.HasPrefix(code, p.code) {
			return p.kind, p.code
		}
	}
	return "", ""
}

// claudeLimitMetricKind is a generic window's metric kind, and the canonical
// period word its label leads with when the entry carries no group.
//
// The `group` decides, because it is the server's own grouping
// ("rows render grouped under it"): session / weekly / daily / monthly map
// straight onto CLI_AGENT_LIMIT_KIND, and so does a group that begins with one
// of them (`five_hour` is a session window). With NO group the kind's own
// prefix names the period (`daily_all` is daily). A group that names no period
// passes through as its own kind, even under a kind whose spelling begins with
// one (`daily_rolling` grouped `rolling` is a "rolling" row): the label leads
// with the group (claudeLimitPeriodWord), and a kind taken from elsewhere would
// age the row by a period its own label does not name. With neither, the kind
// itself is the kind: the vocabulary is open and the receipt does not
// enum-check it. Never "hint": shared-constants reserves it for routing hints,
// and a usage row claiming it would be read as one.
func claudeLimitMetricKind(groupSlug, kindSlug string) (metricKind, period string) {
	groupKind, _ := claudeLimitPeriodOf(groupSlug)
	kindPeriod, _ := claudeLimitPeriodOf(kindSlug)
	switch {
	case groupKind != "":
		metricKind = groupKind
	case groupSlug == "" && kindPeriod != "":
		metricKind = kindPeriod
	default:
		metricKind = claudeClampBytes(firstNonEmpty(groupSlug, kindSlug), claudeWindowMetricKindMaxLen)
	}
	if groupSlug == "" {
		period = kindPeriod
	}
	if metricKind == "hint" {
		metricKind = "provider_hint"
	}
	return metricKind, period
}

// claudeLimitPeriodWord is a label's first word: the period — the entry's group,
// or with none the period its kind names — title-cased ("weekly" -> "Weekly")
// and kept to ONE word ("five_hour" -> "Five-hour"). One word because the Coding
// capacity dialog reads every word after the first as the window's scope
// (codingCapacityWindows.js windowScopeOf), so "Five hour quota" would read as
// a window scoped to "hour quota". "" when there is no period to name.
func claudeLimitPeriodWord(code string) string {
	if code == "" {
		return ""
	}
	word := strings.ReplaceAll(code, "_", "-")
	return strings.ToUpper(word[:1]) + word[1:]
}

// claudeUnscopedWindowLabel labels a window that names no model or surface.
//
//	kind is `<group>_all`, or the group itself  -> "<Period> quota"   ("Daily quota": all models)
//	any other kind under a period               -> "<Period> <rest>"  ("weekly_burst" -> "Weekly burst")
//	no period at all                            -> the kind, humanized ("something_new" -> "Something new")
//
// "quota" is one of the dialog's generic words, so "<Period> quota" reads as
// the all-models budget — which is what an unscoped `_all` window is.
func claudeUnscopedWindowLabel(periodWord, groupSlug, kindSlug string) string {
	if periodWord == "" {
		return claudeClampLabel(claudeHumanizeCode(kindSlug))
	}
	// The period is already the first word, so the kind's own spelling of it is
	// stripped — the group's ("weekly_burst" under "weekly"), or the kind's
	// period prefix when it names the SAME period as the group, or there is no
	// group ("seven_day_extra" -> "extra"). A kind whose prefix names a different
	// period than its group keeps it, rather than silently losing a word.
	rest := kindSlug
	groupPeriod, _ := claudeLimitPeriodOf(groupSlug)
	switch kindPeriod, prefix := claudeLimitPeriodOf(kindSlug); {
	case groupSlug != "" && (rest == groupSlug || strings.HasPrefix(rest, groupSlug+"_")):
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, groupSlug), "_")
	case prefix != "" && (groupSlug == "" || kindPeriod == groupPeriod):
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, prefix), "_")
	}
	if rest == "" || rest == "all" {
		return claudeClampLabel(periodWord + " quota")
	}
	return claudeClampLabel(periodWord + " " + strings.ReplaceAll(rest, "_", " "))
}

// claudeScopedWindowLabel labels a model- or surface-scoped window
// "<Period> <display name>" ("Weekly Haiku 5"): the dialog reads the words
// after the period as the scope's name. With no period the kind stands in, as
// one word, so the scope still reads as the display name alone.
func claudeScopedWindowLabel(periodWord, kindSlug, display string) string {
	if periodWord == "" {
		periodWord = claudeLimitPeriodWord(kindSlug)
	}
	display = claudeWindowDisplayText(display)
	if display == "" {
		return claudeClampLabel(periodWord)
	}
	return claudeClampLabel(periodWord + " " + display)
}

// claudeWindowModelKey is the nested-pool key a model-scoped window names: one
// of Claude Code's `--model` aliases (fable / opus / sonnet / haiku) when the
// model's id or display name names it as a token ("claude-sonnet-5-1",
// "Claude Haiku 5" -> "sonnet", "haiku"), otherwise the id lower-cased when the
// scope carries one, otherwise the display name's first word when it is all
// letters ("Nova 2" -> "nova"), otherwise the whole display name lower-cased.
//
// It is matched EXACTLY (lower-cased) against the models the device discovers
// (annotateModelPoolsFromUsage) and keyed the same way by shared-constants, so
// the alias pass is the fail-safe direction: a key no discovered model carries
// leaves that model's routes unaware of the window (over-admit), while an
// alias at worst over-blocks the model the alias runs. Bounded to the pool
// bound (cliUsageMaxModelPoolLength) on a rune boundary, because a longer key
// is dropped rather than pooled.
//
// The alias pass runs over the id too, before the id itself is taken, though
// the id alone would be the more literal key. The device discovers only the
// aliases (claudeModelAliases), so a full id such as "claude-sonnet-5-1" would
// pool nothing: a spent Sonnet window would not reach the routes that run
// Sonnet. And the key would flip from "sonnet" to that id the day the provider
// starts filling scope.model.id (every reply seen so far sends `id: null`),
// splitting the row's kind+label+model identity between the two spellings.
// An id that names no alias is still the key.
func claudeWindowModelKey(modelID, modelName string) string {
	id := strings.ToLower(claudeWindowDisplayText(modelID))
	name := strings.ToLower(claudeWindowDisplayText(modelName))
	for _, text := range []string{id, name} {
		if alias := claudeModelAliasNamedIn(text); alias != "" {
			return alias
		}
	}
	if id != "" {
		return claudeClampBytes(id, cliUsageMaxModelPoolLength)
	}
	words := strings.Fields(name)
	if len(words) == 0 {
		return ""
	}
	if claudeAllLetters(words[0]) {
		return claudeClampBytes(words[0], cliUsageMaxModelPoolLength)
	}
	return claudeClampBytes(strings.Join(words, " "), cliUsageMaxModelPoolLength)
}

// claudeModelAliasNamedIn is the `claude --model` alias a lower-cased model id
// or display name names as one of its tokens — runs of letters and digits, so
// "claude-sonnet-5-1", "sonnet 5.1" and "opus+" each name theirs — or "".
func claudeModelAliasNamedIn(text string) string {
	tokens := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	for _, token := range tokens {
		for _, alias := range claudeModelAliases {
			if token == alias.ID {
				return alias.ID
			}
		}
	}
	return ""
}

func claudeAllLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

// claudeWindowSlug is an id fragment for a vendor code or name: lower-case ASCII
// letters and digits, every run of anything else one `_`, trimmed, at most
// claudeWindowSlugMaxLen bytes. A value with no ASCII letter or digit at all —
// a non-Latin model name — slugs to a short digest of itself instead of to
// nothing, so two such names cannot collapse into one window.
func claudeWindowSlug(s string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if gap && b.Len() > 0 {
				b.WriteByte('_')
			}
			gap = false
			b.WriteRune(r)
			continue
		}
		gap = true
	}
	out := b.String()
	if len(out) > claudeWindowSlugMaxLen {
		out = strings.TrimRight(out[:claudeWindowSlugMaxLen], "_")
	}
	if out == "" {
		if trimmed := strings.TrimSpace(strings.ToLower(s)); trimmed != "" {
			sum := sha256.Sum256([]byte(trimmed))
			out = "h" + hex.EncodeToString(sum[:4])
		}
	}
	return out
}

// claudeHumanizeCode turns a slug into words with a capital first letter
// ("something_new" -> "Something new").
func claudeHumanizeCode(slug string) string {
	words := strings.ReplaceAll(slug, "_", " ")
	if words == "" {
		return ""
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

// claudeWindowDisplayText is a vendor display name as a label may carry it:
// letters, marks, digits, punctuation and symbols only — control, format and
// other invisible runes dropped — every run of whitespace one space, trimmed.
// The label is published and rendered verbatim, so nothing here may smuggle a
// newline or a bidi override onto the card.
func claudeWindowDisplayText(s string) string {
	var b strings.Builder
	gap := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			gap = true
		case unicode.IsLetter(r), unicode.IsMark(r), unicode.IsNumber(r), unicode.IsPunct(r), unicode.IsSymbol(r):
			if gap && b.Len() > 0 {
				b.WriteByte(' ')
			}
			gap = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// claudeClampLabel bounds a derived label to claudeWindowLabelMaxRunes runes and
// claudeWindowLabelMaxBytes bytes, cutting on a rune boundary.
func claudeClampLabel(label string) string {
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > claudeWindowLabelMaxRunes {
		label = strings.TrimSpace(string([]rune(label)[:claudeWindowLabelMaxRunes]))
	}
	return claudeClampBytes(label, claudeWindowLabelMaxBytes)
}

// claudeClampBytes bounds s to max bytes, cutting on a rune boundary.
func claudeClampBytes(s string, max int) string {
	return strings.TrimSpace(clampAntigravityQuotaField(s, max))
}

/* ─────────────────────────── reply -> buckets + set ─────────────────────────── */

// claudeUsageReplyWindows maps one decoded usage reply onto the cache: the
// readings it carries, and — when the reply is AUTHORITATIVE — every window it
// reports. Shared by the probe and by Claude Code's own cached reply
// (claudeDotJSONFacts), so the two derive the same ids and the same rows from
// the same payload.
//
// Readings (unchanged): limits[] first, then the legacy top-level windows,
// which fill only what the list did not supply; a window with no readable
// percentage writes no bucket; a JSON-null legacy window is skipped and never
// clears anything by itself. Two entries mapping to one id: the first wins.
//
// A reply is authoritative when limits[] decoded as an array (null or absent is
// "a server that predates rows", not "no windows") AND it is non-empty or no
// legacy window produced a reading — `limits: []` beside readable legacy
// windows is a server that has not populated the list, not an account with no
// meters. Its set is every window a limits[] entry names, with or without a
// readable value (the window exists; its row reads Unknown), plus every legacy
// window that produced a reading: absence is believed only when BOTH
// representations omit a window.
//
// Two more things make a reply partial, and a partial reply is never
// authoritative: a malformed entry (no kind, or a scope we could not read),
// which is dropped and so cannot be placed in the set — leaving it out would
// hide whichever window it was; and a list longer than the schema's cap
// (claudeUsageProbeMaxLimits), read only up to the cap.
//
// A partial reply still ADDS what it could read. A reading of a window this
// agent never modelled carries the row it renders as (claudeRateLimitBucket.Row),
// so it is shown whether or not any set names it: a new meter at 100 % beside a
// malformed entry must reach the card and routing, not sit in the cache with
// nothing to call it until a fully readable reply arrives.
func claudeUsageReplyWindows(decoded claudeUsageProbeResponse, nowMs int64) (map[string]claudeRateLimitBucket, *claudeWindowSet) {
	buckets := map[string]claudeRateLimitBucket{}
	reported := []claudeReportedWindow{}
	seen := map[string]bool{}
	partial := len(decoded.Limits) > claudeUsageProbeMaxLimits
	var generic []string
	for i, limit := range decoded.Limits {
		if i >= claudeUsageProbeMaxLimits {
			break
		}
		w, ok := limit.reportedWindow()
		if !ok {
			partial = true
			continue
		}
		if seen[w.ID] {
			continue
		}
		seen[w.ID] = true
		w.Order = len(reported)
		reported = append(reported, w)
		isGeneric := strings.HasPrefix(w.ID, claudeGenericWindowPrefix)
		if isGeneric {
			generic = append(generic, w.ID)
		}
		if bucket, ok := claudeUsageProbeBucket(limit.window(), nowMs); ok {
			if isGeneric {
				row := w
				bucket.Row = &row
			}
			buckets[w.ID] = bucket
		}
	}
	legacyRead := false
	for _, legacy := range []struct {
		id string
		w  *claudeUsageProbeWindow
	}{
		{claudeWindowFiveHour, decoded.FiveHour},
		{claudeWindowSevenDay, decoded.SevenDay},
		{claudeWindowSevenDaySonnet, decoded.SevenDaySonnet},
		{claudeWindowSevenDayOpus, decoded.SevenDayOpus},
		{claudeWindowSevenDayFable, decoded.SevenDayFable},
		{claudeWindowSevenDayOverageIncluded, decoded.SevenDayOverageIncluded},
	} {
		if _, taken := buckets[legacy.id]; taken || legacy.w == nil {
			continue
		}
		bucket, ok := claudeUsageProbeBucket(*legacy.w, nowMs)
		if !ok {
			continue
		}
		buckets[legacy.id] = bucket
		legacyRead = true
		if !seen[legacy.id] {
			seen[legacy.id] = true
			reported = append(reported, claudeReportedWindow{ID: legacy.id, Order: len(reported)})
		}
	}
	logClaudeGenericWindowsOnce(generic)
	if decoded.Limits == nil || partial || (len(decoded.Limits) == 0 && legacyRead) {
		return buckets, nil
	}
	return buckets, &claudeWindowSet{AtMs: nowMs, Windows: reported}
}

// claudeGenericWindowsLogged bounds logClaudeGenericWindowsOnce: one line per
// window id per process, for at most claudeGenericWindowsLogMax ids.
const claudeGenericWindowsLogMax = 16

var claudeGenericWindowsLogged struct {
	mu   sync.Mutex
	seen map[string]bool
}

// logClaudeGenericWindowsOnce names the generic windows a reply reported the
// first time this process sees each one — ids only, never a value, a reset, a
// status or anything from the credential. It is how a new meter (a kind or a
// scope this agent never modelled) becomes diagnosable from agent.log, the way
// logAntigravityQuotaShapeOnce does for Antigravity's replies.
func logClaudeGenericWindowsOnce(ids []string) {
	if len(ids) == 0 {
		return
	}
	logged := &claudeGenericWindowsLogged
	logged.mu.Lock()
	if logged.seen == nil {
		logged.seen = map[string]bool{}
	}
	fresh := make([]string, 0, len(ids))
	for _, id := range ids {
		if logged.seen[id] || len(logged.seen) >= claudeGenericWindowsLogMax {
			continue
		}
		logged.seen[id] = true
		fresh = append(fresh, id)
	}
	logged.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	fmt.Printf("%s[claude-usage] provider reported windows this agent renders generically: %s%s\n",
		colorCyan, strings.Join(fresh, ", "), colorReset)
}

/* ─────────────────────────── which set, which rows ─────────────────────────── */

// claudeEffectiveWindowSet is the window set rows are built from, or nil when
// none is known — never probed successfully, a cache from an older agent, or
// both sources ruled out below — in which case the rows are today's three,
// then any reading of a window this agent never modelled that this login took
// (claudeCodeMetricsFromBuckets).
//
// The NEWER of the two sources wins: the probe's persisted set, and Claude
// Code's own cached reply. Either is a full statement of the account's windows
// at its instant, so the later one is simply the better-informed.
//
// The probe's set is bound to the login it was read under — account AND
// organization (claudeAccountBinding). A claude.ai credential carries no
// account at all, so the cache fingerprint cannot see a `/login` to another
// account, or the same user's switch to another organization, and the previous
// login's set would go on hiding the new one's windows. When the recorded and
// the current login are known to differ, the set is ignored — falling back to
// today's rows, the fail-safe direction — until a probe or Claude Code's own
// reply describes the login signed in now. Claude Code's reply is bound by
// construction (claudeDotJSONFacts takes it only when its accountUuid is the
// signed-in one) and is ignored when it claims a fetch implausibly far in the
// future (claudeWindowSetSkew).
func claudeEffectiveWindowSet(view claudeRateLimitView, now time.Time) *claudeWindowSet {
	best := claudeBoundProbeWindowSet(view)
	if own := view.dot.usageWindows; own != nil && own.AtMs > 0 &&
		own.AtMs <= now.Add(claudeWindowSetSkew).UnixMilli() {
		if best == nil || own.AtMs > best.AtMs {
			best = own
		}
	}
	return best
}

// claudeBoundProbeWindowSet is the probe's persisted set when it may describe
// the account signed in now — recorded under that account, or with either side
// unknown — and nil when there is none or it was read under another account
// (the binding claudeEffectiveWindowSet enforces). The freshness check asks for
// it on its own (stalestClaudeRowObservation): how fresh OUR values are is the
// probe's question, whichever source the rows are built from.
func claudeBoundProbeWindowSet(view claudeRateLimitView) *claudeWindowSet {
	probe := view.reported
	if probe == nil || probe.AtMs <= 0 {
		return nil
	}
	if probe.binding().conflicts(view.dot.binding()) {
		return nil
	}
	return probe
}

// claudeIsFableWindowID reports whether a cache id may hold the weekly Fable
// window: the canonical seven_day_fable, Claude Code's wire key for the same
// meter (seven_day_overage_included), or any other id naming Fable — except a
// five_hour* id (a session window) and a generic window, which are rows of
// their own. The same rule claudeFableWindowIDs applies.
func claudeIsFableWindowID(id string) bool {
	if id == claudeWindowSevenDayFable || id == claudeWindowSevenDayOverageIncluded {
		return true
	}
	lower := strings.ToLower(id)
	return strings.Contains(lower, "fable") &&
		!strings.HasPrefix(lower, claudeWindowFiveHour) &&
		!strings.HasPrefix(lower, claudeGenericWindowPrefix)
}

// claudeCodeMetricsFromWindowSet builds the rows of an account whose window set
// is known.
//
// The windows shown are the set's, plus any window with a reading observed
// AFTER the set (rule 3): an unverified Ultra account that is given a temporary
// session limit, say, shows it as soon as a stream event or a status-line render
// reports it, without waiting for the next probe. Buckets outside those are
// hidden, never deleted (see the file header). Rows, in order:
//
//  1. the session row, when five_hour is shown — the builder and label it always
//     had, Unknown when its bucket is missing or rolled over;
//  2. "Weekly quota", when any of seven_day / seven_day_sonnet / seven_day_opus
//     is shown, aggregated over the shown ones only;
//  3. "Weekly Fable" (model fable), when a Fable window is shown;
//  4. the generic windows, in the server's order (ties by id), from the row
//     their reading carries — else the row the set recorded, for a window the
//     set names without a readable value — and the value of their bucket:
//     Unknown when it is missing, rolled over or never observed;
//  5. any other generic window SHOWN — a reading newer than the set, from a
//     reply that was not authoritative — from the row its reading carries
//     (claudeMetricRows.addGenericReadings). An id with no row at all — an orphan stream
//     id such as `overage` — stays unrendered, as it always was.
//
// At most cliUsageMaxMetricsPerProvider rows, with kind+label+model unique
// (the later duplicate dropped): either violation would fail the whole signed
// refresh. The recorded rows are re-bounded here too, since the cache is a file
// another binary may have written.
func claudeCodeMetricsFromWindowSet(buckets map[string]claudeRateLimitBucket, set *claudeWindowSet, now time.Time) []cliAgentUsageMetric {
	shown := map[string]bool{}
	for _, w := range set.Windows {
		shown[w.ID] = true
	}
	for id, b := range buckets {
		if b.ObservedAtMs > set.AtMs {
			shown[id] = true
		}
	}
	visible := make(map[string]claudeRateLimitBucket, len(shown))
	for id := range shown {
		if b, ok := buckets[id]; ok {
			visible[id] = b
		}
	}

	rows := claudeMetricRows{}
	add := rows.add

	if shown[claudeWindowFiveHour] {
		add(observedMetricOrUnknown(visible, []string{claudeWindowFiveHour}, limitKindSession, claudeSessionLabel, now))
	}
	for _, id := range claudeWeeklyWindowIDs() {
		if shown[id] {
			add(aggregateWeeklyMetric(visible, now))
			break
		}
	}
	// The Fable candidates in claudeFableWindowIDs' order — canonical first, then
	// the sorted variants — but drawn from the windows SHOWN, not from the
	// buckets: a Fable window the set names without a readable value has no
	// bucket and must still render (as Unknown).
	fableIDs := make([]string, 0, 2)
	for _, id := range []string{claudeWindowSevenDayFable, claudeWindowSevenDayOverageIncluded} {
		if shown[id] {
			fableIDs = append(fableIDs, id)
		}
	}
	variants := []string{}
	for id := range shown {
		if id != claudeWindowSevenDayFable && id != claudeWindowSevenDayOverageIncluded && claudeIsFableWindowID(id) {
			variants = append(variants, id)
		}
	}
	sort.Strings(variants)
	fableIDs = append(fableIDs, variants...)
	if len(fableIDs) > 0 {
		fable := observedMetricOrUnknown(visible, fableIDs, limitKindWeekly, claudeFableLabel, now)
		fable.Model = claudeModelFable
		add(fable)
	}

	generic := make([]claudeReportedWindow, 0, len(set.Windows))
	named := map[string]bool{}
	for _, w := range set.Windows {
		if strings.HasPrefix(w.ID, claudeGenericWindowPrefix) {
			named[w.ID] = true
			// The reading's own row first: it was derived from the same reply as
			// the value beside it, so a label and its number always come from
			// one answer. The set's row stands in for a window it names without
			// a readable value (no bucket, so no row to carry).
			if b, ok := visible[w.ID]; ok && b.Row != nil {
				w = b.Row.withID(w.ID, w.Order)
			}
			generic = append(generic, w)
		}
	}
	sortClaudeWindows(generic)
	for _, w := range generic {
		if m, ok := claudeGenericMetric(w, visible, now); ok {
			add(m)
		}
	}
	rows.addGenericReadings(visible, named, now)
	return rows.metrics
}

// claudeMetricRows collects one provider's rows under the signed receipt's two
// rules: at most cliUsageMaxMetricsPerProvider rows, and kind+label+model unique.
// Either violation would fail the WHOLE refresh, for every provider, so both
// row builders add through this.
type claudeMetricRows struct {
	metrics []cliAgentUsageMetric
	// index is each identity's position in metrics.
	index map[string]int
}

// add appends m — or, when a row with the same identity is already in, keeps
// whichever of the two is the better evidence (claudeRowIsBetterEvidence), at
// the earlier row's position. The receipt takes one row per identity, so two
// windows that derive one identity — two kinds of one group scoped to the same
// model, which the label does not tell apart — publish ONE row, and it must be
// the one that constrains: whichever was added first must not win just for
// being first, nor whichever was read last, or a 30 % window hides a spent one
// beside it and routing admits a model that cannot run. The three modelled rows
// never collide (claudeIsFrozenLabel keeps every derived label off theirs).
func (r *claudeMetricRows) add(m cliAgentUsageMetric) {
	identity := m.Kind + "\x00" + m.Label + "\x00" + m.Model
	if r.index == nil {
		r.index = map[string]int{}
	}
	if i, ok := r.index[identity]; ok {
		if claudeRowIsBetterEvidence(m, r.metrics[i]) {
			r.metrics[i] = m
		}
		return
	}
	if len(r.metrics) >= cliUsageMaxMetricsPerProvider {
		return
	}
	r.index[identity] = len(r.metrics)
	r.metrics = append(r.metrics, m)
}

// claudeRowIsBetterEvidence reports whether `candidate` should replace `held`
// as the one row of their shared identity: a reading beats an Unknown
// placeholder, and between two readings the more CONSTRAINING one wins (the
// higher share consumed), the later observation only on a tie — the way the
// weekly row keeps the worst of its windows (aggregateWeeklyMetric). Over-block,
// never over-admit. That is also right for one window read under two cache ids:
// usage within a window only grows, and a reading whose window has rolled over
// is already Unknown.
func claudeRowIsBetterEvidence(candidate, held cliAgentUsageMetric) bool {
	if candidate.Unknown {
		return false
	}
	if held.Unknown {
		return true
	}
	if c, h := claudeRowConsumed(candidate), claudeRowConsumed(held); c != h {
		return c > h
	}
	return observedAtMsFromRFC3339(candidate.ObservedAt) > observedAtMsFromRFC3339(held.ObservedAt)
}

// claudeRowConsumed is a reading's share consumed, 0 when it carries none.
func claudeRowConsumed(m cliAgentUsageMetric) float64 {
	if m.Consumed == nil {
		return 0
	}
	return *m.Consumed
}

// addGenericReadings adds a row for every reading of a window this agent never
// modelled that has not been rendered yet (`skip`), from the row the reading
// carries (claudeRateLimitBucket.Row) — a reply that was not authoritative
// names no set, but what it could read is still a constraint. A reading with no
// row (an orphan id) adds nothing. In the order the readings' replies listed
// them, ties by id, so the same cache always gives the same rows.
func (r *claudeMetricRows) addGenericReadings(buckets map[string]claudeRateLimitBucket, skip map[string]bool, now time.Time) {
	extra := make([]claudeReportedWindow, 0)
	for id, b := range buckets {
		if b.Row == nil || skip[id] || !strings.HasPrefix(id, claudeGenericWindowPrefix) {
			continue
		}
		extra = append(extra, b.Row.withID(id, b.Row.Order))
	}
	sortClaudeWindows(extra)
	for _, w := range extra {
		if m, ok := claudeGenericMetric(w, buckets, now); ok {
			r.add(m)
		}
	}
}

// withID is the row under the id it is stored at — a bucket's row is keyed by
// its bucket, not by a field of its own — at the given position.
func (w claudeReportedWindow) withID(id string, order int) claudeReportedWindow {
	w.ID, w.Order = id, order
	return w
}

// sortClaudeWindows orders generic windows the way the server listed them, ties
// by id: deterministic whatever map they were gathered from.
func sortClaudeWindows(windows []claudeReportedWindow) {
	sort.SliceStable(windows, func(i, j int) bool {
		if windows[i].Order != windows[j].Order {
			return windows[i].Order < windows[j].Order
		}
		return windows[i].ID < windows[j].ID
	})
}

// claudeGenericMetric is the row of one generic window: its recorded kind,
// label and model, re-bounded — the cache is a file another binary may have
// written — and the value of its bucket, Unknown when missing, rolled over or
// never observed. ok is false for a row with no kind or label to show.
func claudeGenericMetric(w claudeReportedWindow, buckets map[string]claudeRateLimitBucket, now time.Time) (cliAgentUsageMetric, bool) {
	kind := claudeClampBytes(w.Kind, claudeWindowMetricKindMaxLen)
	label := claudeClampLabel(w.Label)
	if kind == "" || label == "" {
		return cliAgentUsageMetric{}, false
	}
	if kind == "hint" {
		kind = "provider_hint"
	}
	if claudeIsFrozenLabel(label) {
		label = claudeClampLabel(label + " limit")
	}
	row := observedMetricOrUnknown(buckets, []string{w.ID}, kind, label, now)
	row.Model = claudeClampBytes(strings.ToLower(w.Model), cliUsageMaxModelPoolLength)
	return row, true
}
