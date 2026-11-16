// browser_fill.go — filling a form from the model's values (the POC's driver).
//
// A value names its field by the label the person sees (or name:, id:,
// autocomplete: or a ref from read). Each field is matched, checked against the
// fingerprint it had when the fill started (a page swapping fields under the
// agent stops that field), filled step by step through the kit, and read back.
// The kit refuses hidden, unreadable and sensitive fields and never submits;
// while the fill runs its guards (and, on the DevTools route, a network
// backstop) cancel anything the page tries to submit by itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

type kitOption struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Selected bool   `json:"selected"`
	Disabled bool   `json:"disabled"`
}

type kitSensitivity struct {
	Level string `json:"level"`
	Kind  string `json:"kind"`
}

type kitField struct {
	Ref               string          `json:"ref"`
	Kind              string          `json:"kind"`
	Type              string          `json:"type"`
	Label             string          `json:"label"`
	VisibleLabel      string          `json:"visibleLabel"`
	LabelMismatch     bool            `json:"labelMismatch"`
	Placeholder       string          `json:"placeholder"`
	Name              string          `json:"name"`
	ID                string          `json:"id"`
	Autocomplete      string          `json:"autocomplete"`
	Required          bool            `json:"required"`
	Disabled          bool            `json:"disabled"`
	ReadOnly          bool            `json:"readOnly"`
	Visible           bool            `json:"visible"`
	HiddenReason      string          `json:"hiddenReason"`
	Sensitive         *kitSensitivity `json:"sensitive"`
	Value             string          `json:"value"`
	HasValue          bool            `json:"hasValue"`
	Checked           *bool           `json:"checked"`
	Options           []kitOption     `json:"options"`
	Invalid           bool            `json:"invalid"`
	ValidationMessage string          `json:"validationMessage"`
	FormRef           string          `json:"formRef"`
	FP                string          `json:"fp"`
}

type kitScan struct {
	DocID  string     `json:"docId"`
	URL    string     `json:"url"`
	Title  string     `json:"title"`
	Fields []kitField `json:"fields"`
}

type kitStepResult struct {
	Done       bool            `json:"done"`
	Next       string          `json:"next"`
	WaitMs     int             `json:"waitMs"`
	Outcome    string          `json:"outcome"`
	Detail     json.RawMessage `json:"detail"`
	Method     string          `json:"method"`
	Chosen     string          `json:"chosen"`
	ReadBack   string          `json:"readBack"`
	Validation *string         `json:"validation"`
	Options    []string        `json:"options"`
	InsertText *string         `json:"insertText"`
}

// browserFieldOutcome is one entry of a fill result's `outcomes`.
type browserFieldOutcome struct {
	Field      string   `json:"field"`
	Label      string   `json:"label,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Outcome    string   `json:"outcome"`
	ReadBack   string   `json:"readBack,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Validation string   `json:"validation,omitempty"`
}

type browserFillResult struct {
	TabID              string                `json:"tabId"`
	URL                string                `json:"url"`
	Title              string                `json:"title,omitempty"`
	Outcomes           []browserFieldOutcome `json:"outcomes"`
	Filled             int                   `json:"filled"`
	EndMessage         string                `json:"endMessage"`
	NotResponding      bool                  `json:"notResponding"`
	BlockedSubmits     int                   `json:"blockedSubmits"`
	BackgroundRequests int                   `json:"backgroundRequests"`
	Stopped            bool                  `json:"stopped,omitempty"`
}

func normLabel(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// labelOf is the label a field is matched by. When the page's accessible name
// (aria-*) differs from the label a person sees, only the visible one counts:
// an invisible name must not steer a value into a field the person reads as
// something else.
func labelOf(f kitField) string {
	if f.LabelMismatch && f.VisibleLabel != "" {
		return f.VisibleLabel
	}
	return f.Label
}

// matchField picks the field for one value. form, when set, is the form most of
// the values belong to: a field in another form is not used.
func matchField(fields []kitField, pv browserPlanValue, form string) (*kitField, string, []string) {
	sel := pv.Field
	var level func(f kitField) int
	exact := func(ok bool) int {
		if ok {
			return 3
		}
		return 0
	}
	switch {
	case strings.HasPrefix(sel, "name:"):
		v := sel[5:]
		level = func(f kitField) int { return exact(f.Name != "" && f.Name == v) }
	case strings.HasPrefix(sel, "id:"):
		v := sel[3:]
		level = func(f kitField) int { return exact(f.ID != "" && f.ID == v) }
	case strings.HasPrefix(sel, "autocomplete:"):
		v := strings.ToLower(sel[13:])
		level = func(f kitField) int {
			for _, t := range strings.Fields(strings.ToLower(f.Autocomplete)) {
				if t == v {
					return 3
				}
			}
			return 0
		}
	case strings.HasPrefix(sel, "ref:") || browserFieldRefPattern(sel):
		v := strings.TrimPrefix(sel, "ref:")
		level = func(f kitField) int { return exact(f.Ref == v) }
	default:
		v := normLabel(sel)
		level = func(f kitField) int {
			l := normLabel(labelOf(f))
			switch {
			case l == "":
				return 0
			case l == v:
				return 3
			case strings.HasPrefix(l, v):
				return 2
			case strings.Contains(l, v):
				return 1
			}
			return 0
		}
	}
	var hidden, other []string
	for lv := 3; lv >= 1; lv-- {
		var vis []kitField
		for _, f := range fields {
			if level(f) != lv {
				continue
			}
			if form != "" && f.FormRef != "" && f.FormRef != form {
				other = append(other, labelOf(f))
				continue
			}
			if f.Visible {
				vis = append(vis, f)
			} else {
				hidden = append(hidden, fmt.Sprintf("%s (%s)", labelOf(f), f.HiddenReason))
			}
		}
		if len(vis) == 1 {
			return &vis[0], "", nil
		}
		if len(vis) > 1 {
			var c []string
			for _, f := range vis {
				c = append(c, labelOf(f))
			}
			return nil, browserOutcomeAmbiguous, capStrings(c, 10)
		}
	}
	if len(hidden) > 0 {
		return nil, browserOutcomeOnlyHiddenMatch, capStrings(hidden, 10)
	}
	if len(other) > 0 {
		return nil, browserOutcomeOtherForm, capStrings(other, 10)
	}
	return nil, browserOutcomeNoMatch, nil
}

// browserFieldRefPattern: a field ref as the kit writes it ("f12_k3x9", "r2_k3x9").
func browserFieldRefPattern(s string) bool {
	if len(s) < 4 || (s[0] != 'f' && s[0] != 'r') {
		return false
	}
	i := 1
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 1 || i >= len(s) || s[i] != '_' {
		return false
	}
	tag := s[i+1:]
	if len(tag) < 1 || len(tag) > 8 {
		return false
	}
	for _, r := range tag {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func capStrings(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// dominantForm is the form most of the values land in, when the page has several.
func dominantForm(fields []kitField, values []browserPlanValue) string {
	counts := map[string]int{}
	for _, pv := range values {
		if f, why, _ := matchField(fields, pv, ""); f != nil && why == "" && f.FormRef != "" {
			counts[f.FormRef]++
		}
	}
	if len(counts) <= 1 {
		return ""
	}
	best, n := "", 0
	for k, c := range counts {
		if c > n || (c == n && k < best) {
			best, n = k, c
		}
	}
	return best
}

func detailString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return clipString(s, 200)
	}
	return clipString(string(raw), 200)
}

func clipString(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// fillField runs one field through the kit's steps (start, apply, pick, verify).
func (k *kitRunner) fillField(ctx context.Context, ref string, pv browserPlanValue, trusted bool, stop *atomic.Bool) (kitStepResult, error) {
	step := "start"
	opts := map[string]any{"trustedText": trusted, "sensitiveConfirmed": pv.Confirmed}
	var acc kitStepResult
	for i := 0; i < 160; i++ {
		var r kitStepResult
		if _, err := k.call(ctx, "fillStep", &r, ref, pv.Value, step, opts); err != nil {
			return acc, err
		}
		if r.Chosen != "" {
			// The option the kit picked, by its displayed label: the verify step
			// compares what the field shows against it, not against the value
			// asked for (an option matched by its value, "DE" for "Germany").
			acc.Chosen = r.Chosen
			opts["chosen"] = r.Chosen
		}
		if r.InsertText != nil {
			// A stop that arrived while the kit prepared the field leaves it
			// alone: the text is typed by the agent, so nothing is written yet.
			if k.lastStop || (stop != nil && stop.Load()) {
				acc.Outcome = browserOutcomeStoppedMidField
				return acc, nil
			}
			// Trusted typing through the DevTools Input domain. Check one last time
			// that the field is still visible and focused: a page can hide it on focus.
			var chk struct {
				OK  bool   `json:"ok"`
				Why string `json:"why"`
			}
			if _, err := k.call(ctx, "check", &chk, ref); err != nil {
				return acc, err
			}
			if !chk.OK {
				d, _ := json.Marshal(chk.Why)
				return kitStepResult{Done: true, Outcome: browserOutcomeRefusedHidden, Detail: d}, nil
			}
			if err := k.tr.InsertText(ctx, *r.InsertText); err != nil {
				if isNotResponding(err) {
					k.hung = true
				}
				return acc, err
			}
		}
		if r.Done {
			if r.Chosen == "" {
				r.Chosen = acc.Chosen
			}
			return r, nil
		}
		// A stop leaves alone whatever is not written yet. Verify only reads
		// what was written, so it still runs: the person is told the truth
		// about a value that already landed.
		if (k.lastStop || (stop != nil && stop.Load())) && r.Next != "verify" {
			acc.Outcome = browserOutcomeStoppedMidField
			return acc, nil
		}
		sleepCtx(ctx, r.WaitMs)
		if ctx.Err() != nil {
			return acc, ctx.Err()
		}
		// A stop that arrived during the wait (the highlight delay, a dropdown
		// opening) is honoured before the next step can write anything.
		if stop != nil && stop.Load() && r.Next != "verify" {
			acc.Outcome = browserOutcomeStoppedMidField
			return acc, nil
		}
		step = r.Next
	}
	return acc, errors.New("too many steps")
}

// rescanFor looks again, for about a second, for a field that an earlier answer
// revealed or re-rendered. Fingerprints already planned are kept.
func (k *kitRunner) rescanFor(ctx context.Context, fields *[]kitField, fp map[string]string, docID string, pv browserPlanValue, form string) *kitField {
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		var sc kitScan
		if _, err := k.call(ctx, "scan", &sc); err != nil || sc.DocID != docID {
			return nil
		}
		*fields = sc.Fields
		for _, f := range sc.Fields {
			if _, ok := fp[f.Ref]; !ok {
				fp[f.Ref] = f.FP
			}
		}
		if f, why, _ := matchField(sc.Fields, pv, form); f != nil && why == "" {
			return f
		}
		sleepCtx(ctx, 200)
	}
	return nil
}

type kitEnd struct {
	BlockedSubmits []json.RawMessage `json:"blockedSubmits"`
	Requests       []json.RawMessage `json:"requestsDuringFill"`
}

type kitStatus struct {
	DocID          string            `json:"docId"`
	BlockedSubmits []json.RawMessage `json:"blockedSubmits"`
	Requests       []json.RawMessage `json:"requests"`
}

// browserFillGrace is how long the agent keeps watching after the fill for a
// submit the page starts on a timer (the kit's guards stay armed for 2.5 s).
var browserFillGrace = 2800 * time.Millisecond

// runBrowserFill fills one page. It never returns an error for what happens to
// a field: every field gets an outcome. An error means the page could not be
// read at all.
func runBrowserFill(ctx context.Context, k *kitRunner, values []browserPlanValue, stop *atomic.Bool) (browserFillResult, error) {
	res := browserFillResult{TabID: k.tr.TabID(), Outcomes: []browserFieldOutcome{}}
	var sc kitScan
	if _, err := k.call(ctx, "scan", &sc); err != nil {
		return res, err
	}
	res.URL, res.Title = sc.URL, sc.Title
	docID := sc.DocID
	fields := sc.Fields
	fp := map[string]string{}
	for _, f := range fields {
		fp[f.Ref] = f.FP
	}
	trusted := k.tr.TrustedInput()
	if _, err := k.call(ctx, "begin", nil, map[string]any{"label": "AI Expedite is filling this form"}); err != nil {
		return res, err
	}
	guardOn := k.tr.Guard(ctx, true) == nil

	stopped, pageChanged := false, false
	form := dominantForm(fields, values)
	for _, pv := range values {
		if !stopped && (k.lastStop || stop.Load()) {
			stopped = true
		}
		out := browserFieldOutcome{Field: pv.Field}
		hungBefore := k.hung
		func() {
			if hungBefore {
				out.Outcome = browserOutcomePageNotResponding
				return
			}
			if stopped {
				out.Outcome = browserOutcomeSkippedStopped
				return
			}
			if ctx.Err() != nil {
				out.Outcome = browserOutcomeFailed
				out.Detail = "ran out of time"
				return
			}
			f, why, cands := matchField(fields, pv, form)
			if f == nil && (why == browserOutcomeOnlyHiddenMatch || why == browserOutcomeNoMatch) {
				if nf := k.rescanFor(ctx, &fields, fp, docID, pv, form); nf != nil {
					f, why, cands = nf, "", nil
				}
			}
			if f == nil {
				out.Outcome, out.Candidates = why, cands
				return
			}
			// The field must still be the one that was planned (TOCTOU guard).
			var cur struct {
				Exists bool   `json:"exists"`
				FP     string `json:"fp"`
			}
			if _, err := k.call(ctx, "fingerprint", &cur, f.Ref); err != nil {
				out.Outcome = browserOutcomeFailed
				return
			}
			if !cur.Exists {
				// Re-rendered by the page (a framework replaced the element): match it again.
				if nf := k.rescanFor(ctx, &fields, fp, docID, pv, form); nf != nil && nf.Ref != f.Ref {
					f = nf
					cur.Exists, cur.FP = true, fp[nf.Ref]
				} else {
					out.Outcome = browserOutcomeNotFound
					return
				}
			}
			out.Label, out.Kind = labelOf(*f), f.Kind
			if cur.FP != fp[f.Ref] {
				out.Outcome = browserOutcomeFormChanged
				return
			}
			r, err := k.fillField(ctx, f.Ref, pv, trusted, stop)
			if err != nil {
				out.Outcome = browserOutcomeFailed
				return
			}
			out.Outcome, out.Detail, out.ReadBack = r.Outcome, detailString(r.Detail), clipString(r.ReadBack, 200)
			if f.Sensitive != nil && out.ReadBack != "" {
				out.ReadBack = "•••"
			}
			if r.Validation != nil {
				out.Validation = clipString(*r.Validation, 160)
			}
			if len(r.Options) > 0 {
				out.Candidates = capStrings(r.Options, 25)
			}
		}()
		if k.hung && !hungBefore {
			// It got stuck while working on this field: whether the value landed is unknown.
			out.Outcome = browserOutcomePageNotResponding
		}
		if out.Outcome == "" {
			out.Outcome = browserOutcomeFailed
		}
		if out.Outcome == browserOutcomeFilled || out.Outcome == browserOutcomeFilledThenHidden {
			res.Filled++
		}
		if out.Outcome == browserOutcomeStoppedMidField {
			stopped = true
		}
		res.Outcomes = append(res.Outcomes, out)
		if k.lastStop || stop.Load() {
			stopped = true
		}
		if k.lastDocID != "" && k.lastDocID != docID && !stopped {
			pageChanged = true
			stopped = true
		}
	}

	// Tell the person what happened, without claiming more than is known.
	var pre kitStatus
	if !k.hung {
		_, _ = k.call(ctx, "status", &pre)
	}
	switch {
	case k.hung:
		res.NotResponding = true
		res.EndMessage = "The page stopped responding, so filling stopped. Check the form before it is submitted."
	case pageChanged:
		res.EndMessage = "The page left or submitted the form while filling. Check it before doing anything else."
	case len(pre.BlockedSubmits) > 0:
		res.EndMessage = "Done. The page tried to submit by itself and was stopped. Nothing is submitted without your approval."
	case stopped:
		res.EndMessage = "Stopped. Nothing more will be filled."
	default:
		res.EndMessage = "Done. Check the form. Nothing is submitted without your approval."
	}
	hiddenAfter := 0
	for _, o := range res.Outcomes {
		if o.Outcome == browserOutcomeFilledThenHidden {
			hiddenAfter++
		}
	}
	if hiddenAfter > 0 && !k.hung {
		res.EndMessage += fmt.Sprintf(" The page hid %d field(s) after they were filled; they still hold the values.", hiddenAfter)
	}
	res.Stopped = stopped && !pageChanged

	// Close the window: guards stay armed for the kit's grace period, and so
	// does the network backstop. A submit a timer started by the last change
	// can still fire in the grace (form.submit() into an iframe created then,
	// which neither the kit's submit nor its navigation guard sees); only
	// DevTools' request interception stops that. It is released after the
	// grace, or on any early return.
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*pageCallTimeout)
	defer cancel()
	releaseGuard := func() {
		if guardOn {
			guardOn = false
			_ = k.tr.Guard(endCtx, false)
		}
	}
	defer releaseGuard()
	var e kitEnd
	if !k.hung {
		_, _ = k.call(endCtx, "end", &e, map[string]any{"message": res.EndMessage, "lingerMs": 6000})
	}
	blocked, requests := len(e.BlockedSubmits), len(e.Requests)
	// The page may still try to submit on a timer its last change started.
	if !k.hung && ctx.Err() == nil {
		sleepCtx(ctx, int(browserFillGrace/time.Millisecond))
		var st kitStatus
		if _, err := k.call(endCtx, "status", &st); err == nil {
			if len(st.BlockedSubmits) > blocked {
				blocked = len(st.BlockedSubmits)
			}
			if len(st.Requests) > requests {
				requests = len(st.Requests)
			}
			if st.DocID != docID && !pageChanged {
				res.EndMessage = "The page navigated after the fill. Check it before doing anything else."
			}
		}
	}
	releaseGuard()
	res.BlockedSubmits = blocked + len(k.tr.GuardLog())
	res.BackgroundRequests = requests
	if p, err := k.ping(endCtx); err == nil {
		res.URL, res.Title = p.URL, p.Title
	}
	return res, nil
}
