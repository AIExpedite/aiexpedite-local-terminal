package main

// An opt-in regression run of the ported kit against the proof of concept's
// test and adversarial pages (formfill-poc/testpages), through the agent's own
// DevTools route and fill loop, in a throwaway headless Chrome:
//
//	AIX_BROWSER_SMOKE=1 AIX_BROWSER_POC_ROOT=/path/to/formfill-poc go test -run TestBrowserSmokePOC -v .
//
// Each single-step plan's expected outcomes must hold, and no form submission
// (a document load carrying form data) may reach the server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pocPlan struct {
	Name                string `json:"name"`
	Page                string `json:"page"`
	ReadySelector       string `json:"readySelector"`
	HarnessStopAfter    int    `json:"harnessStopAfter"`
	ExpectStop          bool   `json:"expectStop"`
	ExpectNotResponding bool   `json:"expectNotResponding"`
	Steps               []struct {
		AfterContinue string `json:"afterContinue"`
		Values        []struct {
			Field      string `json:"field"`
			Value      string `json:"value"`
			Expect     string `json:"expect"`
			Force      bool   `json:"force"`
			Confirmed  bool   `json:"confirmed"`
			StopDuring bool   `json:"harnessStopDuring"`
		} `json:"values"`
	} `json:"steps"`
}

var pocPageCSP = map[string]string{
	"/native.html":    "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; form-action 'self'; require-trusted-types-for 'script'; trusted-types 'none'",
	"/hostile.html":   "default-src 'self'; script-src 'self'; style-src 'self'; form-action 'self'",
	"/multistep.html": "default-src 'self'; script-src 'self'; style-src 'self'; form-action 'self'",
	"/big.html":       "default-src 'self'; script-src 'self'; style-src 'self'; form-action 'self'",
	"/react.html":     "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'",
}

func TestBrowserSmokePOC(t *testing.T) {
	chromePath := smokeChromePath(t)
	root := os.Getenv("AIX_BROWSER_POC_ROOT")
	if root == "" {
		t.Skip("set AIX_BROWSER_POC_ROOT to the formfill-poc checkout")
	}
	var mu sync.Mutex
	var navigations, background []string
	files := http.FileServer(http.Dir(filepath.Join(root, "testpages")))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/submitted" {
			dest := r.Header.Get("Sec-Fetch-Dest")
			mu.Lock()
			switch dest {
			case "document", "iframe", "frame", "embed", "object":
				navigations = append(navigations, r.Header.Get("Referer"))
			default:
				background = append(background, r.Header.Get("Referer"))
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><title>Submitted</title><h1>Submitted</h1>")
			return
		}
		if csp, ok := pocPageCSP[r.URL.Path]; ok {
			w.Header().Set("Content-Security-Policy", csp)
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	}))
	defer srv.Close()
	launchSmokeChrome(t, chromePath)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	choice, berr := chooseBrowserRoute(ctx, "darwin", "", discoverChromeDevTools)
	if berr != nil {
		t.Fatal(berr)
	}
	be, release, err := openBrowserBackend(ctx, choice)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	planFiles, _ := filepath.Glob(filepath.Join(root, "plans", "*.json"))
	sort.Strings(planFiles)
	passed, failed := 0, 0
	for _, pf := range planFiles {
		raw, err := os.ReadFile(pf)
		if err != nil {
			t.Fatal(err)
		}
		var plan pocPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatalf("%s: %v", pf, err)
		}
		// The harness stand-ins for the person (Continue, Stop) are not product
		// surface; plans that need them are covered by the POC's own suite.
		if plan.HarnessStopAfter > 0 || plan.ExpectStop || len(plan.Steps) != 1 || plan.Steps[0].AfterContinue != "" {
			continue
		}
		var values []browserPlanValue
		harness := false
		for _, v := range plan.Steps[0].Values {
			harness = harness || v.StopDuring
			values = append(values, browserPlanValue{Field: v.Field, Value: v.Value, Confirmed: v.Confirmed})
		}
		if harness {
			continue
		}
		pg, err := be.Open(ctx, srv.URL+plan.Page)
		if err != nil {
			t.Fatalf("%s: open: %v", plan.Name, err)
		}
		k := newKitRunner(pg)
		if _, err := k.waitLoaded(ctx, "", "", 15*time.Second); err != nil {
			t.Fatalf("%s: load: %v", plan.Name, err)
		}
		if plan.ReadySelector != "" {
			sleepCtx(ctx, 1500) // React mounts after load
		}
		pctx, pcancel := context.WithTimeout(ctx, 3*time.Minute)
		res, err := runBrowserFill(pctx, k, values, &atomic.Bool{})
		pcancel()
		if err != nil {
			t.Errorf("%s: fill: %v", plan.Name, err)
			failed++
			continue
		}
		if res.NotResponding != plan.ExpectNotResponding {
			t.Errorf("%s: notResponding = %v, want %v", plan.Name, res.NotResponding, plan.ExpectNotResponding)
			failed++
		}
		for i, v := range plan.Steps[0].Values {
			if v.Expect == "" || i >= len(res.Outcomes) {
				continue
			}
			wants := strings.Split(v.Expect, "|")
			if v.Force {
				// The POC's "force" fed a hidden field to the kit on purpose; the
				// agent's matcher never offers one, which is the stricter answer.
				wants = append(wants, browserOutcomeOnlyHiddenMatch, browserOutcomeNoMatch)
			}
			ok := false
			for _, w := range wants {
				ok = ok || res.Outcomes[i].Outcome == w
			}
			if ok {
				passed++
			} else {
				failed++
				t.Errorf("%s: %q -> %s (want %s) %s", plan.Name, v.Field, res.Outcomes[i].Outcome, v.Expect, res.Outcomes[i].Detail)
			}
		}
		t.Logf("%s: %d filled, blocked %d, background %d: %s", plan.Name, res.Filled, res.BlockedSubmits, res.BackgroundRequests, res.EndMessage)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("POC plans: %d passed, %d failed; background requests %d", passed, failed, len(background))
	if len(navigations) > 0 {
		t.Fatalf("%d form submission(s) reached the server: %v", len(navigations), navigations)
	}
}
