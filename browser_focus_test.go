package main

import (
	"fmt"
	"testing"
	"time"
)

const testAppOrigin = "https://app.aiexpedite.test"

// tab builds a snapshot tab; f / v / a are "t", "f" or "" (unknown).
func tab(id, url, a, f, v string, window int) browserTabInfo {
	tri := func(s string) *bool {
		switch s {
		case "t":
			return boolPtr(true)
		case "f":
			return boolPtr(false)
		}
		return nil
	}
	return browserTabInfo{TabID: id, Title: "Title " + id, URL: url, Active: tri(a), Focused: tri(f), Visible: tri(v), Window: window}
}

const (
	mailURL = "https://mail.example.com/inbox"
	docsURL = "https://docs.example.com/d/1"
	newsURL = "https://news.example.com/"
	appURL  = testAppOrigin + "/ws/chat"
)

// continuousMemory: switches recorded by events while every switch was seen.
func continuousMemory(at time.Time, tabIDs ...string) *browserFocusMemory {
	m := &browserFocusMemory{}
	m.startContinuous(at.Add(-time.Hour))
	for i, id := range tabIDs {
		m.record(id, at.Add(time.Duration(i)*time.Second), true)
	}
	return m
}

func TestResolveCurrentPage(t *testing.T) {
	now := time.Now()
	type want struct {
		tab   string // resolved tab, or "" for a refusal
		basis string
		code  string
		first string // the first candidate of a refusal
	}
	cases := []struct {
		name string
		snap browserFocusSnapshot
		mem  *browserFocusMemory
		want want
	}{
		{
			name: "the active tab of one window has focus; the inactive ones do not",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Source: browserFocusSourceTabActive, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "f", "f", "f", 0),
				tab("d:B", docsURL, "t", "t", "t", 0),
				tab("d:C", newsURL, "f", "f", "f", 0),
			}},
			want: want{tab: "d:B", basis: browserCurrentBasisFocused},
		},
		{
			name: "multiple windows: the focused tab is in the back window",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "t", "f", "t", 1),
				tab("d:B", docsURL, "f", "f", "f", 1),
				tab("d:C", newsURL, "t", "t", "t", 2),
			}},
			want: want{tab: "d:C", basis: browserCurrentBasisFocused},
		},
		{
			name: "focus change: memory recorded X, then the app tab -> lastActive X",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:X", mailURL, "f", "f", "f", 0),
				tab("d:Y", docsURL, "f", "f", "f", 0),
				tab("d:APP", appURL, "t", "t", "t", 0),
			}},
			mem:  continuousMemory(now, "d:Y", "d:X", "d:APP"),
			want: want{tab: "d:X", basis: browserCurrentBasisLastActive},
		},
		{
			name: "focus change with a connection close in between -> ambiguous, X offered first",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:Y", docsURL, "f", "f", "f", 0),
				tab("d:X", mailURL, "f", "f", "f", 0),
				tab("d:APP", appURL, "t", "t", "t", 0),
			}},
			mem: func() *browserFocusMemory {
				m := continuousMemory(now, "d:Y", "d:X", "d:APP")
				m.interrupt()
				return m
			}(),
			want: want{code: browserErrCurrentPageAmbiguous, first: "d:X"},
		},
		{
			name: "unsupported Chrome (no embedderData): the page probe alone decides",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Source: browserFocusSourcePageProbe, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "f", "f", 0),
				tab("d:B", docsURL, "", "t", "t", 0),
			}},
			want: want{tab: "d:B", basis: browserCurrentBasisFocused},
		},
		{
			name: "unavailable: every probe timed out",
			snap: browserFocusSnapshot{Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "", "", 0),
				tab("d:B", docsURL, "", "", "", 0),
			}},
			want: want{code: browserErrCurrentPageUnavailable, first: "d:A"},
		},
		{
			name: "unavailable: no web page open",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:N", "chrome://newtab/", "t", "t", "t", 0),
			}},
			want: want{code: browserErrCurrentPageUnavailable},
		},
		{
			name: "ambiguous: two visible windows, none focused",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "f", "t", 0),
				tab("d:B", docsURL, "", "f", "t", 0),
				tab("d:C", newsURL, "", "f", "f", 0),
			}},
			want: want{code: browserErrCurrentPageAmbiguous, first: "d:A"},
		},
		{
			name: "ambiguous: the visible page is not the last active one",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "f", "t", 0),
				tab("d:B", docsURL, "", "f", "f", 0),
			}},
			mem:  continuousMemory(now, "d:B"),
			want: want{code: browserErrCurrentPageAmbiguous, first: "d:A"},
		},
		{
			name: "the visible page and the last active one agree -> visible",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "f", "t", 0),
				tab("d:B", docsURL, "", "f", "f", 0),
			}},
			mem:  continuousMemory(now, "d:A"),
			want: want{tab: "d:A", basis: browserCurrentBasisVisible},
		},
		{
			name: "inconsistent: two focused reports",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "t", "t", 0),
				tab("d:B", docsURL, "", "t", "t", 0),
			}},
			want: want{code: browserErrCurrentPageAmbiguous},
		},
		{
			name: "inconsistent: the app tab and a page both focused",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "t", "t", 0),
				tab("d:APP", appURL, "", "t", "t", 0),
			}},
			want: want{code: browserErrCurrentPageAmbiguous},
		},
		{
			name: "the app tab focused with exactly one other page showing -> visible",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:APP", appURL, "t", "t", "t", 1),
				tab("d:A", mailURL, "t", "f", "t", 2),
				tab("d:B", docsURL, "f", "f", "f", 2),
			}},
			want: want{tab: "d:A", basis: browserCurrentBasisVisible},
		},
		{
			name: "incomplete snapshot: a focused tab found is conclusive",
			snap: browserFocusSnapshot{HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "", "", 0),
				tab("d:B", docsURL, "", "t", "t", 0),
			}},
			want: want{tab: "d:B", basis: browserCurrentBasisFocused},
		},
		{
			name: "incomplete snapshot: a lone visible tab is not",
			snap: browserFocusSnapshot{HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "", "", 0),
				tab("d:B", docsURL, "", "f", "t", 0),
			}},
			want: want{code: browserErrCurrentPageAmbiguous, first: "d:B"},
		},
		{
			name: "incomplete snapshot, but the app tab holds focus: last active still decides",
			snap: browserFocusSnapshot{HasSignal: true, Tabs: []browserTabInfo{
				tab("d:A", mailURL, "", "", "", 0),
				tab("d:B", docsURL, "", "f", "f", 0),
				tab("d:APP", appURL, "", "t", "t", 0),
			}},
			mem:  continuousMemory(now, "d:B", "d:APP"),
			want: want{tab: "d:B", basis: browserCurrentBasisLastActive},
		},
		{
			name: "the app tab is never the answer, even focused and alone",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:APP", appURL, "t", "t", "t", 0),
			}},
			want: want{code: browserErrCurrentPageUnavailable},
		},
		{
			name: "the last active tab was closed: nothing older is guessed",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:Y", docsURL, "f", "f", "f", 0),
				tab("d:APP", appURL, "t", "t", "t", 0),
			}},
			mem:  continuousMemory(now, "d:Y", "d:GONE", "d:APP"),
			want: want{code: browserErrCurrentPageAmbiguous},
		},
		{
			name: "a last active record from a snapshot (not an event) is only offered",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
				tab("d:B", docsURL, "f", "f", "f", 0),
				tab("d:X", mailURL, "f", "f", "f", 0),
				tab("d:APP", appURL, "t", "t", "t", 0),
			}},
			mem: func() *browserFocusMemory {
				m := &browserFocusMemory{}
				m.startContinuous(now.Add(-time.Hour))
				m.record("d:X", now, false)
				return m
			}(),
			want: want{code: browserErrCurrentPageAmbiguous, first: "d:X"},
		},
		{
			name: "AppleScript, Chrome not in front: window 1's active tab is the last active page",
			snap: browserFocusSnapshot{Complete: true, HasSignal: true, Source: browserFocusSourceWindowOrder, WindowOrderLast: "a:2", Tabs: []browserTabInfo{
				tab("a:1", mailURL, "f", "f", "f", 1),
				tab("a:2", docsURL, "t", "f", "", 1),
				tab("a:3", newsURL, "t", "f", "", 2),
			}},
			want: want{tab: "a:2", basis: browserCurrentBasisLastActive},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, berr := resolveCurrentPage(c.snap, testAppOrigin, c.mem)
			if c.want.tab != "" {
				if berr != nil {
					t.Fatalf("refused: %v", berr)
				}
				if res.Tab.TabID != c.want.tab || res.Basis != c.want.basis {
					t.Fatalf("got %s/%s, want %s/%s", res.Tab.TabID, res.Basis, c.want.tab, c.want.basis)
				}
				return
			}
			if berr == nil {
				t.Fatalf("resolved %s (%s), want %s", res.Tab.TabID, res.Basis, c.want.code)
			}
			if berr.Code != c.want.code {
				t.Fatalf("code %s, want %s (%s)", berr.Code, c.want.code, berr.Message)
			}
			for _, cand := range berr.Candidates {
				if cand.URL == appURL {
					t.Fatal("the app's own tab must never be a candidate")
				}
			}
			if c.want.first != "" && (len(berr.Candidates) == 0 || berr.Candidates[0].TabID != c.want.first) {
				t.Fatalf("first candidate %+v, want %s", berr.Candidates, c.want.first)
			}
		})
	}
}

// Without an app origin (an older frontend), nothing is marked app: the chat
// tab can resolve as focused, and the card shows its title.
func TestResolveCurrentPageWithoutAppOrigin(t *testing.T) {
	snap := browserFocusSnapshot{Complete: true, HasSignal: true, Tabs: []browserTabInfo{
		tab("d:APP", appURL, "t", "t", "t", 0),
		tab("d:A", mailURL, "f", "f", "f", 0),
	}}
	res, berr := resolveCurrentPage(snap, "", nil)
	if berr != nil || res.Tab.TabID != "d:APP" {
		t.Fatalf("%+v %v", res, berr)
	}
}

// An explicitly named app tab is still reachable: tabs lists it, current never
// returns it.
func TestAppTabStaysListedByTabs(t *testing.T) {
	be := &fakeBackend{route: browserRouteDevTools, tabs: []browserTabInfo{tab("d:APP", appURL, "", "", "", 0)}}
	r := &browserOpRun{p: browserParams{Op: browserOpTabs}, be: be}
	res, err := r.run(t.Context())
	if err != nil || len(res.(browserTabsResult).Tabs) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestResolveCurrentPageResultShape(t *testing.T) {
	now := time.Now()
	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	snap := browserFocusSnapshot{Complete: true, HasSignal: true, Source: browserFocusSourceTabActive, ChromeVersion: "Chrome/150.0.1.2", Tabs: []browserTabInfo{
		{TabID: "d:A", Title: string(long), URL: mailURL, Active: boolPtr(true), Focused: boolPtr(false), Visible: boolPtr(false)},
		tab("d:APP", appURL, "t", "t", "t", 0),
	}}
	res, berr := resolveCurrentPage(snap, testAppOrigin+"/", continuousMemory(now, "d:A", "d:APP"))
	if berr != nil {
		t.Fatal(berr)
	}
	if res.Tab.Title != clipString(string(long), 300) || len(res.Tab.Title) >= 500 || res.Source != browserFocusSourceTabActive || res.ChromeVersion != "Chrome/150.0.1.2" {
		t.Fatalf("%+v", res)
	}
	if res.Focused == nil || res.Focused.TabID != "d:APP" {
		t.Fatalf("focused should name the tab holding focus: %+v", res.Focused)
	}
	if res.LastActive == nil || res.LastActive.TabID != "d:A" || res.LastActive.ObservedAt != now.UnixMilli() {
		t.Fatalf("%+v", res.LastActive)
	}
}

func TestCurrentCandidatesOrderAndCap(t *testing.T) {
	var tabs []browserTabInfo
	for i := 0; i < 14; i++ {
		tabs = append(tabs, tab("d:"+string(rune('a'+i)), mailURL, "f", "f", "f", 0))
	}
	tabs = append(tabs, tab("d:act", docsURL, "t", "f", "f", 0), tab("d:vis", newsURL, "t", "f", "t", 0))
	isApp := func(browserTabInfo) bool { return false }
	got := currentCandidates(tabs, isApp, "d:c")
	if len(got) != browserMaxCurrentCandidates {
		t.Fatalf("%d candidates", len(got))
	}
	if got[0].TabID != "d:vis" || got[1].TabID != "d:c" || !got[1].LastActive || got[2].TabID != "d:act" {
		t.Fatalf("order: %+v", got[:3])
	}
}

func TestBrowserFocusMemory(t *testing.T) {
	t0 := time.Now()
	m := &browserFocusMemory{}
	known := func(id string) (bool, bool) { return id != "d:closed", id == "d:app" }
	if _, _, ok := m.recall(known); ok {
		t.Fatal("empty memory recalls nothing")
	}
	m.record("d:a", t0, true)
	if _, cont, ok := m.recall(known); !ok || cont {
		t.Fatal("not continuous before discovery started")
	}
	m.startContinuous(t0.Add(time.Second))
	m.record("d:b", t0.Add(2*time.Second), true)
	m.record("d:app", t0.Add(3*time.Second), true)
	e, cont, ok := m.recall(known)
	if !ok || !cont || e.TabID != "d:b" {
		t.Fatalf("%+v %v %v", e, cont, ok)
	}
	m.interrupt()
	if _, cont, _ := m.recall(known); cont {
		t.Fatal("an interruption ends continuity")
	}
	m.startContinuous(t0.Add(4 * time.Second))
	if _, cont, _ := m.recall(known); cont {
		t.Fatal("a record from before the new span is not continuous")
	}
	for i := 0; i < 2*browserFocusMaxEntries; i++ {
		m.record("d:"+string(rune('a'+i%20)), t0, true)
	}
	if len(m.ring) != browserFocusMaxEntries {
		t.Fatalf("ring %d", len(m.ring))
	}
	if browserFocusMemoryFor("k") != browserFocusMemoryFor("k") || browserFocusMemoryFor("k") == browserFocusMemoryFor("other") {
		t.Fatal("one memory per Chrome instance")
	}
	first := browserFocusMemoryFor("instance-0")
	for i := 1; i <= browserFocusMaxMemories; i++ {
		browserFocusMemoryFor(fmt.Sprintf("instance-%d", i))
	}
	if len(browserFocusMemories) > browserFocusMaxMemories || browserFocusMemoryFor("instance-0") == first {
		t.Fatal("restarted Chromes' memories are bounded; the oldest is forgotten")
	}
	var nilMem *browserFocusMemory
	nilMem.record("d:a", t0, true)
	nilMem.interrupt()
}

func TestBrowserOriginOf(t *testing.T) {
	for raw, want := range map[string]string{
		"https://App.Example.com/x?y": "https://app.example.com",
		"http://localhost:3000/":      "http://localhost:3000",
		"chrome://newtab":             "chrome://newtab",
		"not a url":                   "",
	} {
		if got := browserOriginOf(raw); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}
