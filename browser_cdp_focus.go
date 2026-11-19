// browser_cdp_focus.go — focus on the DevTools route (the `current` op).
//
// Two signals, neither trusted alone:
//
//   - Chrome's `tab` targets carry an experimental, undocumented
//     `embedderData` object; Chrome 150+ is reported to put a boolean
//     `tabActive` in it (the tab showing in its window). It is feature-detected
//     on every snapshot (tabActiveUsable) and only narrows which pages the
//     probe asks. It says nothing about which WINDOW has focus.
//   - The page probe asks each page `document.hasFocus()` and
//     `document.visibilityState` in the kit's isolated world. It works on every
//     Chrome this route supports (144+) and is the authority on focus.
//
// When the two disagree (tabActive narrowed the set and no probed page is
// focused or visible), every page is probed. The observed behaviour per Chrome
// version is recorded in BROWSER_CONTROL.md "Which page" (the headed smoke
// matrix, browser_smoke_current_test.go).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Probe bounds (variables so tests can shorten them).
var (
	// browserProbeMaxPages: pages probed at most; past it the snapshot is incomplete.
	browserProbeMaxPages = 30
	// browserProbeParallel: probes in flight at once.
	browserProbeParallel = 6
	// browserProbePageLimit: one page's answer, attaching included.
	browserProbePageLimit = 1500 * time.Millisecond
	// browserProbeBudget: the whole probe (a re-probe included).
	browserProbeBudget = 5 * time.Second
)

// browserFocusProbeJS runs in the kit's isolated world: same document, none of
// the page's own scripts.
const browserFocusProbeJS = `JSON.stringify({f: document.hasFocus(), v: document.visibilityState})`

func (e cdpEndpoint) focusKey() string {
	return fmt.Sprintf("%s:%d%s", browserRouteDevTools, e.Port, e.WSPath)
}

// parseFocusProbe reads one probe answer. Anything that does not parse counts
// as unknown (nil), never as false.
func parseFocusProbe(raw string) (focused, visible *bool) {
	var r struct {
		F *bool   `json:"f"`
		V *string `json:"v"`
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return nil, nil
	}
	if r.V != nil {
		switch *r.V {
		case "visible":
			visible = boolPtr(true)
		case "hidden":
			visible = boolPtr(false)
		}
	}
	return r.F, visible
}

// embedderTabActive reads `embedderData.tabActive`: ok only for a boolean.
func embedderTabActive(raw json.RawMessage) (active, ok bool) {
	if len(raw) == 0 {
		return false, false
	}
	var d map[string]json.RawMessage
	if json.Unmarshal(raw, &d) != nil {
		return false, false
	}
	switch string(d["tabActive"]) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// isWindowTab: a tab target that is a tab of a browser window. Chrome also
// reports an extension's background page as a "tab" target, with no
// embedderData and no web URL; that one is not a tab anybody looks at.
func isWindowTab(t cdpTarget) bool {
	return t.Type == "tab" && (len(t.EmbedderData) > 0 || isBrowserHTTPURL(t.URL))
}

// linkPagesToTabs maps tab target id -> page target id. A page names its tab
// in parentId where Chrome sets it; Chrome 157 does not (the ids differ and
// parentId is empty), so a tab and a page are also linked when they show the
// same URL and no other unlinked tab or page shows it. A URL two tabs share is
// never guessed at: those tabs stay unlinked.
func linkPagesToTabs(tabs map[string]string, pages map[string]cdpTarget) map[string]string {
	pageOf := map[string]string{}
	linkedPage := map[string]bool{}
	for id, p := range pages {
		if _, ok := tabs[p.ParentID]; ok && p.ParentID != "" {
			pageOf[p.ParentID] = id
			linkedPage[id] = true
		}
	}
	tabsByURL := map[string][]string{}
	for id, u := range tabs {
		if _, ok := pageOf[id]; !ok {
			tabsByURL[u] = append(tabsByURL[u], id)
		}
	}
	pagesByURL := map[string][]string{}
	for id, p := range pages {
		if !linkedPage[id] {
			pagesByURL[p.URL] = append(pagesByURL[p.URL], id)
		}
	}
	for u, ts := range tabsByURL {
		if ps := pagesByURL[u]; u != "" && len(ts) == 1 && len(ps) == 1 {
			pageOf[ts[0]] = ps[0]
		}
	}
	return pageOf
}

// cdpLinkedTargets is one Target.getTargets answer, pages linked to their tab.
type cdpLinkedTargets struct {
	Pages []cdpTarget
	// Active: page target id -> its tab's tabActive (only when Usable).
	Active map[string]bool
	// Unlinked: web pages whose tab could not be told (two tabs on one URL).
	// Nothing is assumed about them: the probe asks each one.
	Unlinked map[string]bool
	// Usable: every window tab carries a boolean tabActive and at least one
	// linked tab is active. The schema has no window id, so the number of
	// active tabs is not checked.
	Usable bool
}

func linkTabTargets(infos []cdpTarget) cdpLinkedTargets {
	out := cdpLinkedTargets{Active: map[string]bool{}, Unlinked: map[string]bool{}}
	notUsable := func() cdpLinkedTargets {
		return cdpLinkedTargets{Pages: out.Pages, Active: map[string]bool{}, Unlinked: map[string]bool{}}
	}
	tabs := map[string]cdpTarget{}
	tabURLs := map[string]string{}
	pages := map[string]cdpTarget{}
	for _, t := range infos {
		switch {
		case t.Type == "page":
			out.Pages = append(out.Pages, t)
			pages[t.TargetID] = t
		case isWindowTab(t):
			tabs[t.TargetID] = t
			tabURLs[t.TargetID] = t.URL
		}
	}
	if len(tabs) == 0 {
		return out
	}
	pageOf := linkPagesToTabs(tabURLs, pages)
	tabOfPage := map[string]string{}
	for tab, page := range pageOf {
		tabOfPage[page] = tab
	}
	anyActive := false
	for id, tab := range tabs {
		active, ok := embedderTabActive(tab.EmbedderData)
		if !ok {
			return notUsable()
		}
		if page, linked := pageOf[id]; linked {
			out.Active[page] = active
			anyActive = anyActive || active
		}
	}
	for _, p := range out.Pages {
		if _, ok := tabOfPage[p.TargetID]; !ok && isBrowserHTTPURL(p.URL) {
			out.Unlinked[p.TargetID] = true
		}
	}
	out.Usable = anyActive
	if !out.Usable {
		return notUsable()
	}
	return out
}

// countTabActive: how many window tabs are active (the number of windows),
// ok only when every window tab reports tabActive.
func countTabActive(infos []cdpTarget) (active int, ok bool) {
	tabs := 0
	for _, t := range infos {
		if !isWindowTab(t) {
			continue
		}
		tabs++
		a, has := embedderTabActive(t.EmbedderData)
		if !has {
			return 0, false
		}
		if a {
			active++
		}
	}
	return active, tabs > 0
}

// focusProbe answers one page; nil, nil when it did not answer.
type focusProbe func(ctx context.Context, targetID string) (focused, visible *bool)

// buildCDPFocusSnapshot turns a target list and a probe into a snapshot.
func buildCDPFocusSnapshot(ctx context.Context, infos []cdpTarget, cheapOnly bool, probe focusProbe) (browserFocusSnapshot, error) {
	linked := linkTabTargets(infos)
	if cheapOnly && !linked.Usable {
		return browserFocusSnapshot{}, errBrowserFocusNotCheap
	}
	ctx, cancel := context.WithTimeout(ctx, browserProbeBudget)
	defer cancel()

	snap := browserFocusSnapshot{Source: browserFocusSourcePageProbe}
	index := map[string]int{}
	var web []string
	for _, p := range linked.Pages {
		index[p.TargetID] = len(snap.Tabs)
		t := browserTabInfo{TabID: makeBrowserTabID(browserRouteDevTools, p.TargetID), Title: p.Title, URL: p.URL}
		snap.Tabs = append(snap.Tabs, t)
		if isBrowserHTTPURL(p.URL) {
			web = append(web, p.TargetID)
		}
	}
	probed := map[string]bool{}
	answered := map[string]bool{}
	run := func(ids []string) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		slots := make(chan struct{}, browserProbeParallel)
		for _, id := range ids {
			if len(probed) >= browserProbeMaxPages {
				break
			}
			probed[id] = true
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-slots }()
				pctx, pcancel := context.WithTimeout(ctx, browserProbePageLimit)
				f, v := probe(pctx, id)
				pcancel()
				mu.Lock()
				defer mu.Unlock()
				t := &snap.Tabs[index[id]]
				t.Focused, t.Visible = f, v
				if f != nil || v != nil {
					answered[id] = true
				}
			}(id)
		}
		wg.Wait()
	}

	if linked.Usable {
		snap.Source = browserFocusSourceTabActive
		// Each window's active page, and every page whose tab is not known.
		var active []string
		for _, id := range web {
			if linked.Unlinked[id] {
				active = append(active, id)
				continue
			}
			a := linked.Active[id]
			t := &snap.Tabs[index[id]]
			t.Active = boolPtr(a)
			if a {
				active = append(active, id)
			} else {
				// A tab not showing in its window is hidden and has no focus.
				t.Focused, t.Visible = boolPtr(false), boolPtr(false)
			}
		}
		run(active)
		seen := false
		for _, id := range active {
			t := snap.Tabs[index[id]]
			seen = seen || isTrue(t.Focused) || isTrue(t.Visible)
		}
		if !seen && !cheapOnly {
			// Cross-check: no active page is focused or showing. Chrome may be
			// minimized, or tabActive may not mean what it seemed to: ask
			// every page, and trust only their answers.
			snap.Source = browserFocusSourcePageProbe
			var rest []string
			for _, id := range web {
				t := &snap.Tabs[index[id]]
				t.Active = nil
				if !probed[id] {
					t.Focused, t.Visible = nil, nil
					rest = append(rest, id)
				}
			}
			run(rest)
		}
	} else {
		run(web)
	}

	snap.Complete = true
	for _, id := range web {
		t := snap.Tabs[index[id]]
		if t.Focused == nil {
			snap.Complete = false
		}
	}
	snap.HasSignal = len(answered) > 0
	return snap, nil
}

// focusSnapshot lists tab and page targets and probes them (see the top).
func (cn *cdpConn) focusSnapshot(ctx context.Context, cheapOnly bool) (browserFocusSnapshot, error) {
	var r struct {
		TargetInfos []cdpTarget `json:"targetInfos"`
	}
	err := cn.c.Call(ctx, "", "Target.getTargets", map[string]any{
		"filter": []map[string]any{{"type": "tab"}, {"type": "page"}},
	}, &r)
	if err != nil {
		if ctx.Err() != nil {
			return browserFocusSnapshot{}, err
		}
		// A Chrome that refuses the filter: pages only (no tabActive).
		pages, perr := cn.targets(ctx)
		if perr != nil {
			return browserFocusSnapshot{}, perr
		}
		r.TargetInfos = pages
	}
	if active, ok := countTabActive(r.TargetInfos); !ok || active != 1 || cn.disc.unresolved() {
		// Not one window right now, or the latest switch went to a page that
		// could not be told: what the memory holds may miss a switch.
		cn.mem.interrupt()
	}
	snap, err := buildCDPFocusSnapshot(ctx, r.TargetInfos, cheapOnly, cn.probeFocus)
	if err != nil {
		return snap, err
	}
	snap.ChromeVersion = cn.chromeVersion(ctx)
	return snap, nil
}

func (cn *cdpConn) probeFocus(ctx context.Context, targetID string) (focused, visible *bool) {
	cn.mu.Lock()
	pg := cn.pages[targetID]
	cn.mu.Unlock()
	if pg == nil || pg.detached.Load() {
		var err error
		if pg, err = cn.attach(ctx, targetID); err != nil {
			return nil, nil
		}
	}
	raw, err := pg.Eval(ctx, browserFocusProbeJS)
	if err != nil {
		return nil, nil
	}
	return parseFocusProbe(raw)
}

// chromeVersion reads Chrome's product string once per connection (reported
// with `current` and logged; nothing is gated on it).
func (cn *cdpConn) chromeVersion(ctx context.Context) string {
	cn.versionOnce.Do(func() {
		var v struct {
			Product string `json:"product"`
		}
		if cn.c.Call(ctx, "", "Browser.getVersion", nil, &v) == nil {
			cn.version = clipString(v.Product, 80)
		}
	})
	return cn.version
}

// ---------------------------------------------------------------- discovery

// cdpDiscovery is what Target.setDiscoverTargets reported on this connection:
// each tab target's tabActive and its page. A tab whose tabActive flips to
// true is an activation the focus memory records.
type cdpDiscovery struct {
	mu        sync.Mutex
	on        bool                 // the initial burst is over; flips are activations
	tabActive map[string]bool      // window tab id -> tabActive
	tabURL    map[string]string    // window tab id -> its URL
	pages     map[string]cdpTarget // page target id -> its last info
	// pending: an activation whose page target was not announced yet.
	pendingTab string
	pendingAt  time.Time
	// unsettled: the last event left other than one active tab. A switch
	// inside a window passes through that for one event (the old tab's flip
	// to false and the new one's to true arrive one after the other); two
	// events in a row mean a second window (or none).
	unsettled bool
}

func (d *cdpDiscovery) init() {
	if d.tabActive == nil {
		d.tabActive, d.tabURL, d.pages = map[string]bool{}, map[string]string{}, map[string]cdpTarget{}
	}
}

// pageOfLocked: the page target a tab shows, when it can be told (see linkPagesToTabs).
func (d *cdpDiscovery) pageOfLocked(tabID string) string {
	return linkPagesToTabs(d.tabURL, d.pages)[tabID]
}

// resolvePendingLocked records a pending activation once its page is known.
func (d *cdpDiscovery) resolvePendingLocked(mem *browserFocusMemory) {
	if d.pendingTab == "" {
		return
	}
	if page := d.pageOfLocked(d.pendingTab); page != "" {
		mem.record(makeBrowserTabID(browserRouteDevTools, page), d.pendingAt, true)
		d.pendingTab = ""
	}
}

// windowsLocked: tabs showing in a window, so the number of windows.
func (d *cdpDiscovery) windowsLocked() int {
	n := 0
	for _, a := range d.tabActive {
		if a {
			n++
		}
	}
	return n
}

// continuityLocked: every switch is seen only with ONE window (no event
// reports a change of window focus). Called after every event.
func (d *cdpDiscovery) continuityLocked(at time.Time, mem *browserFocusMemory) {
	switch {
	case !d.on:
		mem.interrupt()
	case d.windowsLocked() == 1:
		d.unsettled = false
		mem.startContinuous(at)
	case d.unsettled:
		mem.interrupt()
	default:
		d.unsettled = true
	}
}

func (d *cdpDiscovery) target(t cdpTarget, at time.Time, mem *browserFocusMemory) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	switch {
	case t.Type == "page":
		d.pages[t.TargetID] = t
		// Only a page that could be the pending tab's is worth a new match:
		// title and load updates of every other page stay O(1).
		if d.pendingTab != "" && (t.ParentID == d.pendingTab || t.URL == d.tabURL[d.pendingTab]) {
			d.resolvePendingLocked(mem)
		}
		d.continuityLocked(at, mem)
	case isWindowTab(t):
		active, ok := embedderTabActive(t.EmbedderData)
		if !ok {
			// No usable tabActive: switches cannot be seen.
			d.on = false
			mem.interrupt()
			return
		}
		was, had := d.tabActive[t.TargetID]
		d.tabActive[t.TargetID] = active
		d.tabURL[t.TargetID] = t.URL
		if d.on && active && (!had || !was) {
			if d.pendingTab != "" && d.pendingTab != t.TargetID {
				// The previous switch never reached the memory (its page could
				// not be told): the memory's newest record is not where the
				// person went, so it is not continuous any more.
				mem.interrupt()
			}
			d.pendingTab, d.pendingAt = t.TargetID, at
		}
		d.resolvePendingLocked(mem)
		d.continuityLocked(at, mem)
	}
}

func (d *cdpDiscovery) destroyed(targetID string, at time.Time, mem *browserFocusMemory) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	if _, ok := d.pages[targetID]; ok {
		delete(d.pages, targetID)
		return
	}
	if _, ok := d.tabActive[targetID]; !ok {
		return
	}
	delete(d.tabActive, targetID)
	delete(d.tabURL, targetID)
	if d.pendingTab == targetID {
		d.pendingTab = ""
	}
	d.continuityLocked(at, mem)
}

// unresolved: the latest switch went to a tab whose page is not known yet.
func (d *cdpDiscovery) unresolved() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pendingTab != ""
}

// started marks the initial burst over: from now on, a flip is an activation.
func (d *cdpDiscovery) started(at time.Time, mem *browserFocusMemory) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	d.on = len(d.tabActive) > 0
	d.unsettled = d.windowsLocked() != 1 // a second window at the start is not a transient
	d.continuityLocked(at, mem)
}

// startDiscovery turns on target discovery for this connection. Chrome
// announces every existing target before it answers, so the answer ends the
// initial burst. A Chrome that refuses leaves the memory non-continuous.
func (cn *cdpConn) startDiscovery() {
	err := cn.c.callWithin(context.Background(), "", "Target.setDiscoverTargets", map[string]any{
		"discover": true,
		"filter":   []map[string]any{{"type": "tab"}, {"type": "page"}},
	}, nil, 5*time.Second)
	if err != nil {
		cn.mem.interrupt()
		return
	}
	cn.disc.started(time.Now(), cn.mem)
}
