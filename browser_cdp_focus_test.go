package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func tabTarget(id, embedder string) cdpTarget {
	t := cdpTarget{TargetID: id, Type: "tab"}
	if embedder != "" {
		t.EmbedderData = json.RawMessage(embedder)
	}
	return t
}

func pageTarget(id, parent, url string) cdpTarget {
	return cdpTarget{TargetID: id, Type: "page", ParentID: parent, URL: url, Title: "T " + id}
}

func TestEmbedderTabActive(t *testing.T) {
	for raw, want := range map[string][2]bool{
		`{"tabActive":true}`:            {true, true},
		`{"tabActive":false,"other":1}`: {false, true},
		`{"tabActive":"true"}`:          {false, false},
		`{"tabActive":null}`:            {false, false},
		`{}`:                            {false, false},
		`{"somethingElse":true}`:        {false, false},
		`[]`:                            {false, false},
		``:                              {false, false},
	} {
		a, ok := embedderTabActive(json.RawMessage(raw))
		if a != want[0] || ok != want[1] {
			t.Errorf("%s: %v %v, want %v", raw, a, ok, want)
		}
	}
}

func TestLinkTabTargets(t *testing.T) {
	usable := []cdpTarget{
		tabTarget("T1", `{"tabActive":true}`), tabTarget("T2", `{"tabActive":false}`),
		pageTarget("P1", "T1", mailURL), pageTarget("P2", "T2", docsURL),
		pageTarget("DEV", "", "devtools://devtools/x"), // a page without a tab, not a web page
		{TargetID: "W", Type: "service_worker"},
	}
	l := linkTabTargets(usable)
	if !l.Usable || !l.Active["P1"] || l.Active["P2"] || len(l.Pages) != 3 {
		t.Fatalf("%+v", l)
	}
	notUsable := map[string][]cdpTarget{
		"no tab targets (older Chrome)": {pageTarget("P1", "", mailURL)},
		"missing key":                   {tabTarget("T1", `{}`), pageTarget("P1", "T1", mailURL)},
		"no embedderData":               {tabTarget("T1", ""), pageTarget("P1", "T1", mailURL)},
		"non-boolean":                   {tabTarget("T1", `{"tabActive":1}`), pageTarget("P1", "T1", mailURL)},
		"only tabs on one shared URL (no linked tab is active)": {tabWithURL("T1", `{"tabActive":true}`, mailURL),
			tabWithURL("T2", `{"tabActive":false}`, mailURL), pageTarget("P1", "", mailURL), pageTarget("P2", "", mailURL)},
		"none active": {tabTarget("T1", `{"tabActive":false}`), pageTarget("P1", "T1", mailURL)},
		"one tab of two without the key": {tabTarget("T1", `{"tabActive":true}`), tabTarget("T2", `{}`),
			pageTarget("P1", "T1", mailURL), pageTarget("P2", "T2", docsURL)},
	}
	for name, infos := range notUsable {
		if l := linkTabTargets(infos); l.Usable || len(l.Active) != 0 {
			t.Errorf("%s: %+v", name, l)
		}
	}
}

func tabWithURL(id, embedder, url string) cdpTarget {
	t := tabTarget(id, embedder)
	t.URL = url
	return t
}

// Chrome 157 as observed (BROWSER_CONTROL.md "Which page"): tab and page
// targets have different ids and no parentId; each tab target carries its URL
// and embedderData {tabActive, tabPinned, tabStripIndex}; an extension's
// background page is a "tab" target with no embedderData.
func TestLinkTabTargetsChrome157Shape(t *testing.T) {
	infos := []cdpTarget{
		pageTarget("PA1", "", mailURL), pageTarget("PA2", "", docsURL), pageTarget("PBL", "", "about:blank"),
		tabWithURL("TA1", `{"tabActive":false,"tabPinned":false,"tabStripIndex":0}`, mailURL),
		tabWithURL("TA2", `{"tabActive":true,"tabPinned":false,"tabStripIndex":1}`, docsURL),
		tabWithURL("TBL", `{"tabActive":true,"tabPinned":false,"tabStripIndex":0}`, "about:blank"),
		tabWithURL("EXT", "", "chrome-extension://abc/_generated_background_page.html"),
	}
	l := linkTabTargets(infos)
	if !l.Usable || !l.Active["PA2"] || l.Active["PA1"] {
		t.Fatalf("%+v", l)
	}
	if n, ok := countTabActive(infos); !ok || n != 2 {
		t.Fatalf("windows %d %v (the extension tab is not a window tab)", n, ok)
	}
}

// Tabs that cannot be linked (two on one URL) never make the snapshot fall back
// to probing everything: those pages are probed, the rest still narrowed.
func TestLinkTabTargetsKeepsNarrowingAroundUnlinkedPages(t *testing.T) {
	fastProbe(t)
	infos := []cdpTarget{
		tabWithURL("TA", `{"tabActive":true}`, docsURL), pageTarget("PA", "", docsURL),
		tabWithURL("TB", `{"tabActive":false}`, newsURL), pageTarget("PB", "", newsURL),
		tabWithURL("T1", `{"tabActive":false}`, mailURL), pageTarget("P1", "", mailURL),
		tabWithURL("T2", `{"tabActive":true}`, mailURL), pageTarget("P2", "", mailURL),
		pageTarget("PX", "", "https://no-tab.example.com/"),
	}
	l := linkTabTargets(infos)
	if !l.Usable || !l.Active["PA"] || l.Active["PB"] || !l.Unlinked["P1"] || !l.Unlinked["P2"] || !l.Unlinked["PX"] {
		t.Fatalf("%+v", l)
	}
	p := &fakeProbe{answers: map[string][2]*bool{
		"PA": {no(), yes()}, "P1": {no(), no()}, "P2": {yes(), yes()}, "PX": {no(), no()},
	}}
	snap, err := buildCDPFocusSnapshot(t.Context(), infos, false, p.probe)
	if err != nil {
		t.Fatal(err)
	}
	asked := p.askedSet()
	if asked["PB"] || !asked["PA"] || !asked["P1"] || !asked["P2"] || !asked["PX"] {
		t.Fatalf("probe the active and the unlinked pages only: %v", p.asked)
	}
	if snapTab(snap, "d:P1").Active != nil || snap.Source != browserFocusSourceTabActive || !snap.Complete {
		t.Fatalf("an unlinked page carries no tabActive: %+v", snap)
	}
	res, berr := resolveCurrentPage(snap, "", nil)
	if berr != nil || res.Tab.TabID != "d:P2" {
		t.Fatalf("%+v %v", res, berr)
	}
}

// A switch to a tab whose page cannot be told (two tabs on one URL) never lets
// the memory answer with the tab the person left.
func TestCDPDiscoveryUnresolvedSwitchEndsContinuity(t *testing.T) {
	t0 := time.Now().Add(-time.Second)
	mem := &browserFocusMemory{}
	cn := &cdpConn{mem: mem}
	ev := func(info cdpTarget) {
		raw, _ := json.Marshal(map[string]any{"targetInfo": info})
		cn.onEvent("Target.targetInfoChanged", "", raw)
	}
	for _, info := range []cdpTarget{
		tabWithURL("TX", `{"tabActive":false}`, docsURL), pageTarget("PX", "", docsURL),
		tabWithURL("TD1", `{"tabActive":false}`, mailURL), pageTarget("PD1", "", mailURL),
		tabWithURL("TD2", `{"tabActive":true}`, mailURL), pageTarget("PD2", "", mailURL),
		tabWithURL("TAPP", `{"tabActive":false}`, appURL), pageTarget("PAPP", "", appURL),
	} {
		ev(info)
	}
	cn.disc.started(t0, mem)
	// X (recorded), then one of the two mail tabs (unlinkable), then the chat.
	ev(tabWithURL("TD2", `{"tabActive":false}`, mailURL))
	ev(tabWithURL("TX", `{"tabActive":true}`, docsURL))
	if e, cont, ok := mem.recall(func(string) (bool, bool) { return true, false }); !ok || !cont || e.TabID != "d:PX" {
		t.Fatalf("X is a recorded, continuous switch: %+v %v %v", e, cont, ok)
	}
	ev(tabWithURL("TX", `{"tabActive":false}`, docsURL))
	ev(tabWithURL("TD1", `{"tabActive":true}`, mailURL))
	if !cn.disc.unresolved() {
		t.Fatal("the switch to a shared-URL tab is unresolved")
	}
	ev(tabWithURL("TD1", `{"tabActive":false}`, mailURL))
	ev(tabWithURL("TAPP", `{"tabActive":true}`, appURL))
	open := func(id string) (bool, bool) { return id != "", id == "d:PAPP" }
	if e, cont, ok := mem.recall(open); ok && cont {
		t.Fatalf("lastActive must not be %s: the person left it for a mail tab", e.TabID)
	}
}

func TestParseFocusProbe(t *testing.T) {
	cases := map[string][2]string{ // raw -> focused, visible ("t", "f", "")
		`{"f":true,"v":"visible"}`:    {"t", "t"},
		`{"f":false,"v":"hidden"}`:    {"f", "f"},
		`{"f":false,"v":"prerender"}`: {"f", ""},
		`{"v":"visible"}`:             {"", "t"},
		`not json`:                    {"", ""},
		`{"f":"yes"}`:                 {"", ""},
	}
	show := func(b *bool) string {
		if b == nil {
			return ""
		}
		if *b {
			return "t"
		}
		return "f"
	}
	for raw, want := range cases {
		f, v := parseFocusProbe(raw)
		if show(f) != want[0] || show(v) != want[1] {
			t.Errorf("%s: %s %s, want %v", raw, show(f), show(v), want)
		}
	}
}

// fakeProbe answers from a table; a missing page never answers.
type fakeProbe struct {
	mu      sync.Mutex
	answers map[string][2]*bool
	asked   []string
	wait    time.Duration
}

func (p *fakeProbe) probe(ctx context.Context, id string) (*bool, *bool) {
	p.mu.Lock()
	p.asked = append(p.asked, id)
	a, ok := p.answers[id]
	p.mu.Unlock()
	if !ok || p.wait > 0 {
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(p.wait):
		}
		if !ok {
			return nil, nil
		}
	}
	return a[0], a[1]
}

func (p *fakeProbe) askedSet() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]bool{}
	for _, id := range p.asked {
		out[id] = true
	}
	return out
}

func fastProbe(t *testing.T) {
	t.Helper()
	limit, budget := browserProbePageLimit, browserProbeBudget
	browserProbePageLimit, browserProbeBudget = 60*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { browserProbePageLimit, browserProbeBudget = limit, budget })
}

func yes() *bool { return boolPtr(true) }
func no() *bool  { return boolPtr(false) }

func snapTab(s browserFocusSnapshot, id string) browserTabInfo {
	for _, t := range s.Tabs {
		if t.TabID == id {
			return t
		}
	}
	return browserTabInfo{}
}

func TestCDPFocusSnapshotNarrowsWithTabActive(t *testing.T) {
	fastProbe(t)
	infos := []cdpTarget{
		tabTarget("T1", `{"tabActive":true}`), tabTarget("T2", `{"tabActive":false}`), tabTarget("T3", `{"tabActive":true}`),
		pageTarget("P1", "T1", mailURL), pageTarget("P2", "T2", docsURL), pageTarget("P3", "T3", newsURL),
	}
	p := &fakeProbe{answers: map[string][2]*bool{"P1": {no(), yes()}, "P3": {yes(), yes()}}}
	snap, err := buildCDPFocusSnapshot(t.Context(), infos, false, p.probe)
	if err != nil {
		t.Fatal(err)
	}
	if asked := p.askedSet(); len(asked) != 2 || asked["P2"] {
		t.Fatalf("only one page per window is probed: %v", p.asked)
	}
	if snap.Source != browserFocusSourceTabActive || !snap.Complete || !snap.HasSignal {
		t.Fatalf("%+v", snap)
	}
	if p2 := snapTab(snap, "d:P2"); !(p2.Active != nil && !*p2.Active) || isTrue(p2.Focused) || isTrue(p2.Visible) || p2.Focused == nil {
		t.Fatalf("an inactive tab is known hidden and unfocused: %+v", p2)
	}
	res, berr := resolveCurrentPage(snap, "", nil)
	if berr != nil || res.Tab.TabID != "d:P3" || res.Basis != browserCurrentBasisFocused {
		t.Fatalf("%+v %v", res, berr)
	}
	// Cheap (the tabs op): the same narrowing.
	if _, err := buildCDPFocusSnapshot(t.Context(), infos, true, p.probe); err != nil {
		t.Fatal(err)
	}
}

// tabActive present but contradicted by the probe: every page is asked, and
// only their answers count.
func TestCDPFocusSnapshotReprobesWhenTabActiveDisagrees(t *testing.T) {
	fastProbe(t)
	infos := []cdpTarget{
		tabTarget("T1", `{"tabActive":true}`), tabTarget("T2", `{"tabActive":false}`),
		pageTarget("P1", "T1", mailURL), pageTarget("P2", "T2", docsURL),
	}
	p := &fakeProbe{answers: map[string][2]*bool{"P1": {no(), no()}, "P2": {yes(), yes()}}}
	snap, err := buildCDPFocusSnapshot(t.Context(), infos, false, p.probe)
	if err != nil {
		t.Fatal(err)
	}
	if !p.askedSet()["P2"] || snap.Source != browserFocusSourcePageProbe {
		t.Fatalf("re-probe expected: %v %+v", p.asked, snap)
	}
	if snapTab(snap, "d:P1").Active != nil {
		t.Fatal("tabActive is dropped once contradicted")
	}
	res, berr := resolveCurrentPage(snap, "", nil)
	if berr != nil || res.Tab.TabID != "d:P2" {
		t.Fatalf("%+v %v", res, berr)
	}
	// The tabs op never pays the re-probe.
	p2 := &fakeProbe{answers: p.answers}
	if _, err := buildCDPFocusSnapshot(t.Context(), infos, true, p2.probe); err != nil || p2.askedSet()["P2"] {
		t.Fatalf("cheap snapshot re-probed: %v %v", err, p2.asked)
	}
}

func TestCDPFocusSnapshotUnsupportedChrome(t *testing.T) {
	fastProbe(t)
	infos := []cdpTarget{pageTarget("P1", "", mailURL), pageTarget("P2", "", docsURL), pageTarget("N", "", "chrome://newtab/")}
	if _, err := buildCDPFocusSnapshot(t.Context(), infos, true, (&fakeProbe{}).probe); err != errBrowserFocusNotCheap {
		t.Fatalf("tabs must not pay the full probe: %v", err)
	}
	p := &fakeProbe{answers: map[string][2]*bool{"P1": {no(), yes()}, "P2": {no(), no()}}}
	snap, err := buildCDPFocusSnapshot(t.Context(), infos, false, p.probe)
	if err != nil || snap.Source != browserFocusSourcePageProbe || !snap.Complete || p.askedSet()["N"] {
		t.Fatalf("%+v %v %v", snap, err, p.asked)
	}
	res, berr := resolveCurrentPage(snap, "", nil)
	if berr != nil || res.Tab.TabID != "d:P1" || res.Basis != browserCurrentBasisVisible {
		t.Fatalf("%+v %v", res, berr)
	}
}

func TestCDPFocusSnapshotTimeoutsAndScale(t *testing.T) {
	fastProbe(t)
	infos := []cdpTarget{pageTarget("P1", "", mailURL), pageTarget("P2", "", docsURL)}
	snap, err := buildCDPFocusSnapshot(t.Context(), infos, false, (&fakeProbe{}).probe)
	if err != nil || snap.HasSignal || snap.Complete {
		t.Fatalf("%+v %v", snap, err)
	}
	if _, berr := resolveCurrentPage(snap, "", nil); berr == nil || berr.Code != browserErrCurrentPageUnavailable {
		t.Fatalf("all probes timed out: %v", berr)
	}

	// Past the page cap the snapshot is incomplete; concurrency stays bounded.
	max := browserProbeMaxPages
	browserProbeMaxPages = 4
	t.Cleanup(func() { browserProbeMaxPages = max })
	var many []cdpTarget
	answers := map[string][2]*bool{}
	for _, id := range []string{"A", "B", "C", "D", "E", "F"} {
		many = append(many, pageTarget(id, "", "https://"+id+".example.com/"))
		answers[id] = [2]*bool{no(), no()}
	}
	answers["B"] = [2]*bool{no(), yes()}
	var inFlight, peak atomic.Int32
	probe := func(ctx context.Context, id string) (*bool, *bool) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := peak.Load()
			if n <= m || peak.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		a := answers[id]
		return a[0], a[1]
	}
	snap, _ = buildCDPFocusSnapshot(t.Context(), many, false, probe)
	if snap.Complete || peak.Load() > int32(browserProbeParallel) {
		t.Fatalf("complete=%v peak=%d", snap.Complete, peak.Load())
	}
	if _, berr := resolveCurrentPage(snap, "", nil); berr == nil || berr.Code != browserErrCurrentPageAmbiguous {
		t.Fatalf("a lone visible tab in an incomplete snapshot is not conclusive: %v", berr)
	}
}

func TestCDPDiscoveryFeedsFocusMemory(t *testing.T) {
	t0 := time.Now().Add(-time.Second) // discovery started before the switches below
	mem := &browserFocusMemory{}
	cn := &cdpConn{mem: mem}
	ev := func(method string, info cdpTarget) {
		raw, _ := json.Marshal(map[string]any{"targetInfo": info})
		cn.onEvent(method, "", raw)
	}
	// The initial burst seeds the state; it records no activation.
	ev("Target.targetCreated", tabTarget("T1", `{"tabActive":true}`))
	ev("Target.targetCreated", tabTarget("T2", `{"tabActive":false}`))
	ev("Target.targetCreated", pageTarget("P1", "T1", mailURL))
	ev("Target.targetCreated", pageTarget("P2", "T2", docsURL))
	cn.disc.started(t0, mem)
	if len(mem.ring) != 0 || mem.continuousSince.IsZero() {
		t.Fatalf("%+v", mem)
	}
	open := func(id string) (bool, bool) { return id == "d:P1" || id == "d:P2", false }
	// The person switches to P2: a flip to true is an activation.
	ev("Target.targetInfoChanged", tabTarget("T1", `{"tabActive":false}`))
	ev("Target.targetInfoChanged", tabTarget("T2", `{"tabActive":true}`))
	// A title change of the active tab is not a switch.
	ev("Target.targetInfoChanged", tabTarget("T2", `{"tabActive":true}`))
	e, cont, ok := mem.recall(open)
	if !ok || !cont || e.TabID != "d:P2" || len(mem.ring) != 1 {
		t.Fatalf("%+v %v %v ring=%+v", e, cont, ok, mem.ring)
	}
	// A new tab activated before its page is announced is recorded once it is.
	ev("Target.targetCreated", tabTarget("T3", `{"tabActive":true}`))
	ev("Target.targetInfoChanged", tabTarget("T2", `{"tabActive":false}`))
	ev("Target.targetCreated", pageTarget("P3", "T3", newsURL))
	if last := mem.ring[len(mem.ring)-1]; last.TabID != "d:P3" || !last.FromEvent {
		t.Fatalf("%+v", mem.ring)
	}
	// A second window: no event reports window focus, so continuity ends.
	ev("Target.targetCreated", tabTarget("T4", `{"tabActive":true}`))
	if mem.continuousSince.IsZero() {
		t.Fatal("one event off one window is a switch in passing")
	}
	ev("Target.targetCreated", pageTarget("P4", "T4", newsURL))
	if !mem.continuousSince.IsZero() {
		t.Fatal("two windows must end continuity")
	}
	// It closes: continuity resumes from now, older records stay untrusted.
	raw, _ := json.Marshal(map[string]any{"targetId": "T4"})
	cn.onEvent("Target.targetDestroyed", "", raw)
	if mem.continuousSince.IsZero() {
		t.Fatal("one window again")
	}
	if _, cont, _ := mem.recall(func(id string) (bool, bool) { return id == "d:P3", false }); cont {
		t.Fatal("a record from before the second window closed is not continuous")
	}
	// An extension's background "tab" (no embedderData) is not a window tab.
	ev("Target.targetCreated", tabWithURL("EXT", "", "chrome-extension://abc/bg.html"))
	if mem.continuousSince.IsZero() || !cn.disc.on {
		t.Fatal("an extension background target must not end continuity")
	}
	// Chrome 157: no parentId, linked by a unique URL.
	ev("Target.targetCreated", pageTarget("P5", "", "https://five.example.com/"))
	ev("Target.targetCreated", tabWithURL("T5", `{"tabActive":false}`, "https://five.example.com/"))
	ev("Target.targetInfoChanged", tabTarget("T3", `{"tabActive":false}`))
	ev("Target.targetInfoChanged", tabWithURL("T5", `{"tabActive":true}`, "https://five.example.com/"))
	if last := mem.ring[len(mem.ring)-1]; last.TabID != "d:P5" {
		t.Fatalf("URL-linked activation: %+v", mem.ring)
	}
	// A tab target without tabActive (a Chrome that changed it): no continuity.
	ev("Target.targetInfoChanged", tabTarget("T3", `{}`))
	if !mem.continuousSince.IsZero() || cn.disc.on {
		t.Fatal("unusable tabActive ends continuity")
	}
}

// closeAll (and every other close) marks the memory non-continuous.
func TestCDPCloseInterruptsFocusMemory(t *testing.T) {
	url, _ := fakeCDPServer(t)
	m := testCDPManager(url, time.Minute)
	conn, release, err := m.acquire(t.Context(), cdpEndpoint{Port: 9333, WSPath: "/devtools/browser/focus-close"})
	if err != nil {
		t.Fatal(err)
	}
	release()
	conn.mem.startContinuous(time.Now())
	m.closeAll()
	// Read under the lock: the connection's background discovery call fails
	// once closeAll shuts the socket, and interrupts the memory concurrently.
	conn.mem.mu.Lock()
	since := conn.mem.continuousSince
	conn.mem.mu.Unlock()
	if !since.IsZero() {
		t.Fatal("closeAll must interrupt the focus memory")
	}
}

func TestBrowserCDPFocusAllowlist(t *testing.T) {
	for _, m := range []string{"Target.setDiscoverTargets", "Browser.getVersion"} {
		if !cdpAllowlist[m] {
			t.Errorf("%s must be allowed", m)
		}
	}
	for _, m := range []string{"Target.activateTarget", "Browser.close", "Browser.setWindowBounds", "Browser.getWindowForTarget"} {
		if cdpAllowlist[m] {
			t.Errorf("%s must not be allowed", m)
		}
	}
	browserMethods := 0
	for m := range cdpAllowlist {
		if len(m) > 8 && m[:8] == "Browser." {
			browserMethods++
		}
	}
	if browserMethods != 1 {
		t.Fatalf("exactly one Browser.* method, got %d", browserMethods)
	}
}

// A page too slow to make the kit's world within the probe's deadline keeps
// its session: the next probe reuses it instead of attaching another, and no
// detach is sent with an expired context.
func TestCDPAttachKeepsASlowPagesSession(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var m struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &m)
			mu.Lock()
			methods = append(methods, m.Method)
			mu.Unlock()
			if m.Method == "Target.attachToTarget" {
				reply, _ := json.Marshal(map[string]any{"id": m.ID, "result": map[string]any{"sessionId": "S1"}})
				_ = c.Write(context.Background(), websocket.MessageText, reply)
			}
			// Page.getFrameTree (the page's own call) is never answered.
		}
	}))
	t.Cleanup(srv.Close)
	client, err := dialCDP(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.close)
	cn := &cdpConn{c: client, pages: map[string]*cdpPage{}}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	pg, err := cn.attach(ctx, "P1")
	if err != nil || pg == nil || cn.pages["P1"] != pg {
		t.Fatalf("the session is kept for reuse: %v %v", pg, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, m := range methods {
		if m == "Target.detachFromTarget" {
			t.Fatalf("a kept session must not be detached: %v", methods)
		}
	}
}
