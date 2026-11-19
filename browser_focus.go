// browser_focus.go — "which page is the person looking at" (the `current` op).
//
// A route takes a browserFocusSnapshot (every tab with what Chrome said about
// it: active in its window, focused, visible; unknown stays nil, never false).
// resolveCurrentPage turns it into ONE tab and the basis that decided it, or a
// refusal with candidates. It never guesses: the model asks the person.
//
// Rules (first wins; BROWSER_CONTROL.md "Which page"):
//
//  1. Only http(s) tabs count. Tabs on the AI Expedite app's own origin are
//     marked app and are never the answer (the person typing in the chat bar
//     makes that tab the focused one).
//  2. Exactly one non-app tab focused -> focused. More than one tab focused is
//     inconsistent evidence -> ambiguous.
//  3. No non-app tab focused, and that is conclusive (the snapshot is complete,
//     or an app tab holds the focus, which is unique):
//     A = the one non-app visible tab, from a complete snapshot only;
//     B = the last active non-app tab, when every switch since is known.
//     One of them, or both the same -> visible (A) / lastActive (B);
//     both and different -> ambiguous.
//  4. Some signal but no answer -> ambiguous, with candidates.
//  5. No signal at all -> unavailable.
//
// A reviewer who prefers "always ask unless focused" removes rule 3 (the A / B
// block in resolveCurrentPage) and nothing else changes.
package main

import (
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// browserFocusSnapshot is what a route knows about focus right now.
type browserFocusSnapshot struct {
	// Tabs: every tab the route saw, with Active / Focused / Visible / Window
	// set when known.
	Tabs []browserTabInfo
	// Complete: every page that could hold focus answered (a focused tab
	// found in an incomplete snapshot is still conclusive: focus is unique).
	Complete bool
	// Source: browserFocusSource*.
	Source string
	// ChromeVersion: Chrome's product string, when the route read it.
	ChromeVersion string
	// HasSignal: at least one page answered the probe, or the route reports
	// focus itself (AppleScript). Without it the answer is "unavailable".
	HasSignal bool
	// WindowOrderLast (AppleScript, Chrome not frontmost): the front window's
	// active tab. Chrome's own window order is exact, so it counts as a
	// continuous last-active record.
	WindowOrderLast string
}

// errBrowserFocusNotCheap: FocusSnapshot(cheapOnly) would have to probe every
// page; the tabs op lists them plainly instead. Never reported to the cloud.
var errBrowserFocusNotCheap = errors.New("this route cannot report focus cheaply")

// browserCurrentTab is a resolved or candidate page.
type browserCurrentTab struct {
	TabID string `json:"tabId"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type browserLastActive struct {
	TabID      string `json:"tabId"`
	ObservedAt int64  `json:"observedAt"`
}

type browserCurrentResult struct {
	Tab           browserCurrentTab  `json:"tab"`
	Basis         string             `json:"basis"`
	Source        string             `json:"source"`
	Focused       *browserCurrentTab `json:"focused"`
	LastActive    *browserLastActive `json:"lastActive"`
	ChromeVersion string             `json:"chromeVersion,omitempty"`
}

// browserCurrentCandidate is one page a refusal offers the person.
type browserCurrentCandidate struct {
	TabID      string `json:"tabId"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Active     *bool  `json:"active,omitempty"`
	Focused    *bool  `json:"focused,omitempty"`
	Visible    *bool  `json:"visible,omitempty"`
	LastActive bool   `json:"lastActive,omitempty"`
	Window     int    `json:"window,omitempty"`
}

// browserMaxCurrentCandidates: BROWSER_LIMITS.MAX_CURRENT_CANDIDATES.
const browserMaxCurrentCandidates = 10

func isTrue(b *bool) bool  { return b != nil && *b }
func boolPtr(b bool) *bool { return &b }

// browserOriginOf returns a URL's origin as URL.origin writes it, or "".
func browserOriginOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

func clipCurrentTab(t browserTabInfo) browserCurrentTab {
	u := t.URL
	if len(u) > browserMaxURLChars {
		u = u[:browserMaxURLChars]
	}
	return browserCurrentTab{TabID: t.TabID, Title: clipString(t.Title, 300), URL: u}
}

// resolveCurrentPage applies the rules above. mem may be nil (no memory).
func resolveCurrentPage(snap browserFocusSnapshot, appOrigin string, mem *browserFocusMemory) (browserCurrentResult, *browserError) {
	appOrigin = strings.ToLower(strings.TrimSuffix(appOrigin, "/"))
	var tabs []browserTabInfo
	open := map[string]browserTabInfo{}
	isApp := func(t browserTabInfo) bool { return appOrigin != "" && browserOriginOf(t.URL) == appOrigin }
	for _, t := range snap.Tabs {
		if !isBrowserHTTPURL(t.URL) {
			continue
		}
		tabs = append(tabs, t)
		open[t.TabID] = t
	}

	// B: the last active non-app tab, and whether every switch since is known.
	var last *browserFocusEntry
	lastContinuous := false
	if snap.WindowOrderLast != "" {
		if t, ok := open[snap.WindowOrderLast]; ok && !isApp(t) {
			last = &browserFocusEntry{TabID: t.TabID, At: time.Now()}
			lastContinuous = true
		}
	} else if mem != nil {
		if e, cont, ok := mem.recall(func(id string) (known, app bool) {
			t, ok := open[id]
			return ok, ok && isApp(t)
		}); ok {
			last, lastContinuous = &e, cont
		}
	}

	var focused, focusedApp []browserTabInfo
	var visible []browserTabInfo
	for _, t := range tabs {
		if isTrue(t.Focused) {
			if isApp(t) {
				focusedApp = append(focusedApp, t)
			} else {
				focused = append(focused, t)
			}
		}
		if isTrue(t.Visible) && !isApp(t) {
			visible = append(visible, t)
		}
	}
	result := func(t browserTabInfo, basis string) browserCurrentResult {
		r := browserCurrentResult{Tab: clipCurrentTab(t), Basis: basis, Source: snap.Source, ChromeVersion: snap.ChromeVersion}
		if all := append(append([]browserTabInfo{}, focused...), focusedApp...); len(all) == 1 {
			f := clipCurrentTab(all[0])
			r.Focused = &f
		}
		if last != nil {
			r.LastActive = &browserLastActive{TabID: last.TabID, ObservedAt: last.At.UnixMilli()}
		}
		return r
	}
	refuse := func(code, msg string) (browserCurrentResult, *browserError) {
		e := browserErr(code, "%s", msg)
		lastID := ""
		if last != nil {
			lastID = last.TabID
		}
		e.Candidates = currentCandidates(tabs, isApp, lastID)
		return browserCurrentResult{}, e
	}

	nonApp := 0
	for _, t := range tabs {
		if !isApp(t) {
			nonApp++
		}
	}
	if nonApp == 0 {
		return refuse(browserErrCurrentPageUnavailable, "no web page is open in Chrome")
	}

	// Rule 2.
	if len(focused)+len(focusedApp) > 1 {
		return refuse(browserErrCurrentPageAmbiguous, "Chrome reported more than one page with focus")
	}
	if len(focused) == 1 {
		return result(focused[0], browserCurrentBasisFocused), nil
	}

	// Rule 3: no non-app tab is focused, conclusively.
	if snap.Complete || len(focusedApp) == 1 {
		var a, b *browserTabInfo
		if snap.Complete && len(visible) == 1 {
			a = &visible[0]
		}
		if last != nil && lastContinuous {
			if t, ok := open[last.TabID]; ok {
				b = &t
			}
		}
		switch {
		case a != nil && b != nil && a.TabID != b.TabID:
			return refuse(browserErrCurrentPageAmbiguous, "the page showing in Chrome is not the page you were last on")
		case a != nil:
			return result(*a, browserCurrentBasisVisible), nil
		case b != nil:
			return result(*b, browserCurrentBasisLastActive), nil
		}
	}

	// Rules 4 and 5.
	if !snap.HasSignal && snap.WindowOrderLast == "" {
		return refuse(browserErrCurrentPageUnavailable, "Chrome did not say which page has focus")
	}
	return refuse(browserErrCurrentPageAmbiguous, "more than one page could be the one you mean")
}

// currentCandidates lists the non-app tabs, visible first, then the last
// active one, then each window's active tab, at most browserMaxCurrentCandidates.
func currentCandidates(tabs []browserTabInfo, isApp func(browserTabInfo) bool, lastID string) []browserCurrentCandidate {
	type ranked struct {
		c    browserCurrentCandidate
		rank int
		i    int
	}
	var list []ranked
	for i, t := range tabs {
		if isApp(t) {
			continue
		}
		ct := clipCurrentTab(t)
		c := browserCurrentCandidate{TabID: ct.TabID, Title: ct.Title, URL: ct.URL, Active: t.Active, Focused: t.Focused, Visible: t.Visible, LastActive: t.TabID == lastID, Window: t.Window}
		rank := 3
		switch {
		case isTrue(t.Visible):
			rank = 0
		case c.LastActive:
			rank = 1
		case isTrue(t.Active):
			rank = 2
		}
		list = append(list, ranked{c, rank, i})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].rank < list[j].rank })
	out := []browserCurrentCandidate{}
	for _, r := range list {
		if len(out) >= browserMaxCurrentCandidates {
			break
		}
		out = append(out, r.c)
	}
	return out
}

// ---------------------------------------------------------------- focus memory

// browserFocusEntry is one activation the agent saw.
type browserFocusEntry struct {
	TabID string
	At    time.Time
	// FromEvent: Chrome reported the switch as it happened (a tab target's
	// tabActive flipping to true), not a snapshot taken later.
	FromEvent bool
}

// browserFocusMaxEntries bounds the activation ring.
const browserFocusMaxEntries = 50

// browserFocusMemory is the last activations of ONE Chrome instance, in this
// process only: never persisted, never sent except as a result's lastActive.
type browserFocusMemory struct {
	mu   sync.Mutex
	ring []browserFocusEntry
	// continuousSince: since when no switch can have been missed (zero = not
	// continuous). DevTools: discovery on, events arriving, one window.
	continuousSince time.Time
}

// record notes that tabID became the active / focused tab at `at`.
func (m *browserFocusMemory) record(tabID string, at time.Time, fromEvent bool) {
	if m == nil || tabID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.ring); n > 0 && m.ring[n-1].TabID == tabID {
		// The same tab again: keep the first sighting's kind if it was an event.
		m.ring[n-1].At = at
		m.ring[n-1].FromEvent = m.ring[n-1].FromEvent || fromEvent
		return
	}
	m.ring = append(m.ring, browserFocusEntry{TabID: tabID, At: at, FromEvent: fromEvent})
	if len(m.ring) > browserFocusMaxEntries {
		m.ring = m.ring[len(m.ring)-browserFocusMaxEntries:]
	}
}

// startContinuous: from `at` on, every switch is reported (no-op when already continuous).
func (m *browserFocusMemory) startContinuous(at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.continuousSince.IsZero() {
		m.continuousSince = at
	}
}

// interrupt: a switch may be missed from now on (the connection closed, or a
// second window appeared, whose focus changes no event reports).
func (m *browserFocusMemory) interrupt() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.continuousSince = time.Time{}
}

// recall walks back from the newest activation, skipping the app's own tabs.
// It stops at the first other entry: that is the page the person was on. A
// closed tab (or one that is not a web page) is not passed over: the person
// was on it, so nothing older is "the page before the chat". continuous is
// true when the entry came from an event inside the current continuous span.
func (m *browserFocusMemory) recall(lookup func(tabID string) (known, app bool)) (e browserFocusEntry, continuous, ok bool) {
	if m == nil {
		return e, false, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.ring) - 1; i >= 0; i-- {
		known, app := lookup(m.ring[i].TabID)
		if app {
			continue
		}
		if !known {
			return e, false, false
		}
		e = m.ring[i]
		since := m.continuousSince
		return e, !since.IsZero() && e.FromEvent && e.At.After(since), true
	}
	return e, false, false
}

var (
	browserFocusMemoriesMu sync.Mutex
	browserFocusMemories   = map[string]*browserFocusMemory{}
)

// browserFocusMemoryFor returns the memory of one Chrome instance: keyed by its
// DevTools endpoint, or browserRouteAppleScript.
func browserFocusMemoryFor(key string) *browserFocusMemory {
	browserFocusMemoriesMu.Lock()
	defer browserFocusMemoriesMu.Unlock()
	m := browserFocusMemories[key]
	if m == nil {
		m = &browserFocusMemory{}
		browserFocusMemories[key] = m
	}
	return m
}
