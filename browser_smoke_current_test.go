package main

// The headed Chrome version matrix for `current` (BROWSER_CONTROL.md "Which
// page"). Opt-in, never in CI (it needs a display):
//
//	AIX_BROWSER_SMOKE=1 AIX_BROWSER_SMOKE_HEADED=1 CHROME_PATH=<chrome> \
//	  go test -run TestBrowserSmokeCurrent -v .
//
// Run it on Chrome for Testing 144, 149, 150 stable and the beta channel
// before a release, and record the logged table in BROWSER_CONTROL.md: it is
// the validation that `embedderData.tabActive` behaves as relied on.
//
// It launches a throwaway headed Chrome (as browser_smoke_test.go does), opens
// two windows of three tabs each and moves focus with Target.activateTarget.
// That call goes over the TEST's own DevTools connection: it is not on the
// agent's allowlist and never will be.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// smokeRawCDP is the test's own DevTools client (no allowlist).
type smokeRawCDP struct {
	ws   *websocket.Conn
	next atomic.Int64
}

func (c *smokeRawCDP) call(ctx context.Context, method string, params any, out any) error {
	id := c.next.Add(1)
	msg, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err := c.ws.Write(ctx, websocket.MessageText, msg); err != nil {
		return err
	}
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		var r struct {
			ID     int64           `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
		}
		if json.Unmarshal(data, &r) != nil || r.ID != id {
			continue // an event, or another reply
		}
		if r.Error != nil {
			return r.Error
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

func TestBrowserSmokeCurrent(t *testing.T) {
	chromePath := smokeChromePath(t)
	if os.Getenv("AIX_BROWSER_SMOKE_HEADED") != "1" {
		t.Skip("set AIX_BROWSER_SMOKE_HEADED=1 (needs a display): focus and visibility need real windows")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		name := strings.Trim(r.URL.Path, "/")
		fmt.Fprintf(w, `<!doctype html><html><head><title>Page %s</title></head><body><h1>%s</h1></body></html>`, name, name)
	}))
	defer srv.Close()
	launchSmokeChromeWith(t, chromePath, true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ep, err := discoverChromeDevTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.Dial(ctx, ep.wsURL(), &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatal(err)
	}
	ws.SetReadLimit(64 << 20)
	defer ws.Close(websocket.StatusNormalClosure, "done")
	raw := &smokeRawCDP{ws: ws}
	var version struct {
		Product string `json:"product"`
	}
	_ = raw.call(ctx, "Browser.getVersion", nil, &version)

	// Two windows, three tabs each.
	ids := map[string]string{} // page name -> target id
	for _, w := range []string{"a", "b"} {
		for i := 1; i <= 3; i++ {
			name := fmt.Sprintf("%s%d", w, i)
			var r struct {
				TargetID string `json:"targetId"`
			}
			params := map[string]any{"url": srv.URL + "/" + name}
			if i == 1 {
				params["newWindow"] = true
			}
			if err := raw.call(ctx, "Target.createTarget", params, &r); err != nil {
				t.Fatalf("createTarget %s: %v", name, err)
			}
			ids[name] = r.TargetID
			time.Sleep(300 * time.Millisecond)
		}
	}

	// "linux" asks for the DevTools route even on a Mac, where AppleScript
	// would also be offered: this matrix is about tabActive and the page probe.
	choice, berr := chooseBrowserRoute(ctx, "linux", "", discoverChromeDevTools)
	if berr != nil || choice.Route != browserRouteDevTools {
		t.Fatalf("route: %+v %v", choice, berr)
	}
	be, release, err := openBrowserBackend(ctx, choice)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	time.Sleep(500 * time.Millisecond) // discovery starts off the dial path

	t.Logf("| Chrome | moved to | embedderData of its tab | tabActive usable | current | basis | source |")
	for _, name := range []string{"a2", "b3", "a1", "b1", "a3"} {
		if err := raw.call(ctx, "Target.activateTarget", map[string]any{"targetId": ids[name]}, nil); err != nil {
			t.Fatalf("activate %s: %v", name, err)
		}
		time.Sleep(700 * time.Millisecond)
		var targets struct {
			TargetInfos []cdpTarget `json:"targetInfos"`
		}
		if err := raw.call(ctx, "Target.getTargets", map[string]any{"filter": []map[string]any{{"type": "tab"}, {"type": "page"}}}, &targets); err != nil {
			t.Fatalf("getTargets: %v", err)
		}
		// The activated page's tab, linked as the agent links them.
		tabURLs, pages := map[string]string{}, map[string]cdpTarget{}
		embedderOf := map[string]string{}
		for _, tg := range targets.TargetInfos {
			switch {
			case tg.Type == "page":
				pages[tg.TargetID] = tg
			case isWindowTab(tg):
				tabURLs[tg.TargetID], embedderOf[tg.TargetID] = tg.URL, string(tg.EmbedderData)
			}
		}
		embedder := "(no linked tab target)"
		for tab, page := range linkPagesToTabs(tabURLs, pages) {
			if page == ids[name] {
				embedder = embedderOf[tab]
			}
		}
		usable := linkTabTargets(targets.TargetInfos).Usable
		if name == "a2" && os.Getenv("AIX_BROWSER_SMOKE_RAW") == "1" {
			// The raw shape, for the matrix: how tab targets and pages link here.
			for _, tg := range targets.TargetInfos {
				t.Logf("raw target id=%s type=%s parentId=%q embedderData=%s url=%s", tg.TargetID, tg.Type, tg.ParentID, tg.EmbedderData, tg.URL)
			}
		}

		r := &browserOpRun{p: browserParams{Op: browserOpCurrent}, be: be, stop: &atomic.Bool{}}
		res, err := r.run(ctx)
		got, basis, source := "", "", ""
		if cur, ok := res.(browserCurrentResult); ok && err == nil {
			got, basis, source = cur.Tab.TabID, cur.Basis, cur.Source
		} else {
			got = browserCode(err)
		}
		t.Logf("| %s | %s | %s | %v | %s | %s | %s |", version.Product, name, embedder, usable, got, basis, source)

		// The one hard rule: never a wrong page. A refusal is allowed (the
		// window manager may not hand OS focus to the activated window).
		if strings.HasPrefix(got, browserTabPrefixDevTools) && got != makeBrowserTabID(browserRouteDevTools, ids[name]) {
			t.Errorf("after moving to %s, current answered %s (%s)", name, got, basis)
		}
		if usable {
			if a, ok := embedderTabActive(json.RawMessage(embedder)); !ok || !a {
				t.Errorf("tabActive is usable but the activated tab %s reports %s", name, embedder)
			}
		}
	}
}
