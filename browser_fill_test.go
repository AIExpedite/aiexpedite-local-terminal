package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

func TestMatchField(t *testing.T) {
	fields := []kitField{
		{Ref: "f1_a", Label: "Full name", Visible: true, Name: "fullName", ID: "name", FormRef: "F1"},
		{Ref: "f2_a", Label: "Email", Visible: true, Autocomplete: "section-x email", FormRef: "F1"},
		{Ref: "f3_a", Label: "Email (confirm)", Visible: false, HiddenReason: "covered", FormRef: "F1"},
		{Ref: "f4_a", Label: "Phone number", Visible: true, FormRef: "F1"},
		{Ref: "f5_a", Label: "Phone extension", Visible: true, FormRef: "F1"},
		{Ref: "f6_a", Label: "Search", Visible: true, FormRef: "F2"},
		{Ref: "f7_a", Label: "aria says Email", VisibleLabel: "Coupon", LabelMismatch: true, Visible: true, FormRef: "F1"},
		{Ref: "f8_a", Label: "Secret question", Visible: false, HiddenReason: "transparent", FormRef: "F1"},
	}
	cases := []struct {
		sel, form, ref, why string
	}{
		{"Full name", "", "f1_a", ""},
		{"full   NAME", "", "f1_a", ""},
		{"Email", "", "f2_a", ""},              // exact beats the hidden prefix match
		{"name:fullName", "", "f1_a", ""},      // selector forms
		{"id:name", "", "f1_a", ""},            //
		{"autocomplete:email", "", "f2_a", ""}, //
		{"f4_a", "", "f4_a", ""},               // a ref from read
		{"ref:f5_a", "", "f5_a", ""},           //
		{"Phone", "", "", browserOutcomeAmbiguous},
		{"Coupon", "", "f7_a", ""}, // only the visible label counts
		{"aria says Email", "", "", browserOutcomeNoMatch},
		{"Secret question", "", "", browserOutcomeOnlyHiddenMatch},
		{"Search", "F1", "", browserOutcomeOtherForm},
		{"Search", "", "f6_a", ""},
		{"Favourite colour", "", "", browserOutcomeNoMatch},
		{"name:", "", "", browserOutcomeNoMatch},
	}
	for _, c := range cases {
		f, why, _ := matchField(fields, browserPlanValue{Field: c.sel}, c.form)
		ref := ""
		if f != nil {
			ref = f.Ref
		}
		if ref != c.ref || why != c.why {
			t.Errorf("%q in %q: %q %q, want %q %q", c.sel, c.form, ref, why, c.ref, c.why)
		}
	}
	if got := dominantForm(fields, []browserPlanValue{{Field: "Full name"}, {Field: "Email"}, {Field: "Search"}}); got != "F1" {
		t.Errorf("dominantForm = %q", got)
	}
	if got := dominantForm(fields, []browserPlanValue{{Field: "Full name"}}); got != "" {
		t.Errorf("one form: %q", got)
	}
	for s, want := range map[string]bool{"f12_ab3d": true, "r2_x": true, "f_ab": false, "f12": false, "Full name": false, "f1_ABC": false} {
		if browserFieldRefPattern(s) != want {
			t.Errorf("browserFieldRefPattern(%q) != %v", s, want)
		}
	}
}

// A fill through a fake kit: trusted typing goes through Input.insertText after
// a last visibility check, the fingerprint guard stops a swapped field, and a
// stop skips what is left.
func TestRunBrowserFill(t *testing.T) {
	prev := browserFillGrace
	browserFillGrace = time.Millisecond
	t.Cleanup(func() { browserFillGrace = prev })

	stop := &atomic.Bool{}
	insert := "Ada"
	page := &fakePage{id: "d:T1", trusted: true}
	page.kit = map[string]func([]json.RawMessage) any{
		"scan": func([]json.RawMessage) any {
			return map[string]any{"docId": "doc-1", "url": "https://ex.com/", "title": "Ex", "fields": []any{
				map[string]any{"ref": "f1_a", "kind": "text", "label": "Name", "visible": true, "fp": "n1"},
				map[string]any{"ref": "f2_a", "kind": "text", "label": "Card number", "visible": true, "fp": "c1", "sensitive": map[string]any{"level": "never", "kind": "payment-card"}},
				map[string]any{"ref": "f3_a", "kind": "text", "label": "City", "visible": true, "fp": "y1"},
				map[string]any{"ref": "f4_a", "kind": "text", "label": "Zip", "visible": true, "fp": "z1"},
			}}
		},
		"fingerprint": func(args []json.RawMessage) any {
			if string(args[0]) == `"f3_a"` {
				return map[string]any{"exists": true, "fp": "swapped"}
			}
			return map[string]any{"exists": true, "fp": map[string]string{`"f1_a"`: "n1", `"f2_a"`: "c1", `"f4_a"`: "z1"}[string(args[0])]}
		},
		"fillStep": func(args []json.RawMessage) any {
			var step string
			_ = json.Unmarshal(args[2], &step)
			switch {
			case string(args[0]) == `"f2_a"`:
				return map[string]any{"done": true, "outcome": "refused-sensitive", "detail": "payment-card"}
			case step == "start":
				return map[string]any{"done": false, "next": "apply", "waitMs": 1}
			case step == "apply":
				return map[string]any{"done": false, "next": "verify", "waitMs": 1, "insertText": insert}
			}
			stop.Store(true) // the person says stop once the first field's value is typed
			return map[string]any{"done": true, "outcome": "filled", "readBack": "Ada"}
		},
		"check":  func([]json.RawMessage) any { return map[string]any{"ok": true} },
		"status": func([]json.RawMessage) any { return map[string]any{"docId": "doc-1"} },
		"end":    func([]json.RawMessage) any { return map[string]any{"ok": true} },
		"ping":   pingOK,
	}
	k := newKitRunner(page)
	res, err := runBrowserFill(context.Background(), k, []browserPlanValue{
		{Field: "Name", Value: "Ada"}, {Field: "Card number", Value: "4111"}, {Field: "City", Value: "Paris"}, {Field: "Zip", Value: "1"},
	}, stop)
	if err != nil {
		t.Fatal(err)
	}
	// The stop arrived after the first field's value was typed: that value is
	// verified and reported as filled (it is in the field); the rest are
	// skipped.
	want := []string{browserOutcomeFilled, browserOutcomeSkippedStopped, browserOutcomeSkippedStopped, browserOutcomeSkippedStopped}
	for i, o := range res.Outcomes {
		if o.Outcome != want[i] {
			t.Errorf("outcome %d = %s, want %s", i, o.Outcome, want[i])
		}
	}
	if len(page.inserted) != 1 || page.inserted[0] != "Ada" || len(page.called("check")) != 1 {
		t.Fatalf("trusted typing: %v, %d checks", page.inserted, len(page.called("check")))
	}
	if res.EndMessage != "Stopped. Nothing more will be filled." || !res.Stopped {
		t.Fatalf("end: %q", res.EndMessage)
	}

	// Without a stop: sensitive refused, the swapped field is not touched.
	stop.Store(false)
	page.kit["fillStep"] = func(args []json.RawMessage) any {
		if string(args[0]) == `"f2_a"` {
			return map[string]any{"done": true, "outcome": "refused-sensitive", "detail": "payment-card"}
		}
		return map[string]any{"done": true, "outcome": "filled", "readBack": "x"}
	}
	res, err = runBrowserFill(context.Background(), newKitRunner(page), []browserPlanValue{
		{Field: "Name", Value: "Ada"}, {Field: "Card number", Value: "4111"}, {Field: "City", Value: "Paris"}, {Field: "Nope", Value: "1"},
	}, stop)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{browserOutcomeFilled, "refused-sensitive", browserOutcomeFormChanged, browserOutcomeNoMatch}
	for i, o := range res.Outcomes {
		if o.Outcome != want[i] {
			t.Errorf("outcome %d = %s, want %s", i, o.Outcome, want[i])
		}
	}
	if res.Filled != 1 || res.EndMessage != "Done. Check the form. Nothing is submitted without your approval." {
		t.Fatalf("%+v", res)
	}
	if res.Outcomes[1].Detail != "payment-card" || res.Outcomes[0].Label != "Name" {
		t.Fatalf("%+v", res.Outcomes)
	}
}

// An option matched by its value ("DE") is shown by its label ("Germany"): the
// label the kit chose travels to the verify step, which compares against it.
func TestBrowserFillFieldForwardsChosenLabel(t *testing.T) {
	var verifyOpts map[string]any
	page := &fakePage{id: "d:T1", trusted: true}
	page.kit = map[string]func([]json.RawMessage) any{
		"fillStep": func(args []json.RawMessage) any {
			var step string
			_ = json.Unmarshal(args[2], &step)
			if step == "start" {
				return map[string]any{"done": false, "next": "verify", "waitMs": 1, "method": "setter", "chosen": "Germany"}
			}
			_ = json.Unmarshal(args[3], &verifyOpts)
			return map[string]any{"done": true, "outcome": "filled", "readBack": "Germany"}
		},
		"ping": pingOK,
	}
	k := newKitRunner(page)
	r, err := k.fillField(context.Background(), "f1_a", "", browserPlanValue{Field: "Country", Value: "DE"}, true, nil)
	if err != nil || r.Outcome != "filled" {
		t.Fatalf("%+v %v", r, err)
	}
	if verifyOpts["chosen"] != "Germany" {
		t.Fatalf("verify step options %v, want chosen Germany", verifyOpts)
	}
}

// A stop that arrives while the kit waits (the highlight delay before apply) or
// while it prepares a field for typing leaves that field alone: no apply runs
// after the wait, and no text is typed.
func TestBrowserFillFieldHonoursStopBeforeWriting(t *testing.T) {
	stop := &atomic.Bool{}
	page := &fakePage{id: "d:T1", trusted: true}
	page.kit = map[string]func([]json.RawMessage) any{
		"fillStep": func(args []json.RawMessage) any {
			var step string
			_ = json.Unmarshal(args[2], &step)
			if step == "start" {
				go func() { time.Sleep(10 * time.Millisecond); stop.Store(true) }()
				return map[string]any{"done": false, "next": "apply", "waitMs": 80}
			}
			return map[string]any{"done": false, "next": "verify", "waitMs": 1, "insertText": "Ada"}
		},
		"check": func([]json.RawMessage) any { return map[string]any{"ok": true} },
		"ping":  pingOK,
	}
	r, err := newKitRunner(page).fillField(context.Background(), "f1_a", "", browserPlanValue{Field: "Name", Value: "Ada"}, true, stop)
	if err != nil || r.Outcome != browserOutcomeStoppedMidField {
		t.Fatalf("stop during the wait: %+v %v", r, err)
	}
	for _, c := range page.called("fillStep") {
		if string(c.args[2]) == `"apply"` {
			t.Fatal("apply ran after a stop arrived during the wait")
		}
	}

	// The stop lands while apply prepares the field: nothing is typed.
	stop.Store(false)
	page2 := &fakePage{id: "d:T1", trusted: true}
	page2.kit = map[string]func([]json.RawMessage) any{
		"fillStep": func(args []json.RawMessage) any {
			stop.Store(true)
			return map[string]any{"done": false, "next": "verify", "waitMs": 1, "insertText": "Ada"}
		},
		"check": func([]json.RawMessage) any { return map[string]any{"ok": true} },
		"ping":  pingOK,
	}
	r, err = newKitRunner(page2).fillField(context.Background(), "f1_a", "", browserPlanValue{Field: "Name", Value: "Ada"}, true, stop)
	if err != nil || r.Outcome != browserOutcomeStoppedMidField || len(page2.inserted) != 0 {
		t.Fatalf("stop before typing: %+v %v inserted=%v", r, err, page2.inserted)
	}
}

// The identity the plan was matched to travels with every step, so the kit can
// refuse a field the page repurposed between the agent's check and the write.
func TestBrowserFillFieldSendsPlannedIdentity(t *testing.T) {
	var seen []string
	page := &fakePage{id: "d:T1", trusted: true}
	page.kit = map[string]func([]json.RawMessage) any{
		"fillStep": func(args []json.RawMessage) any {
			var opts map[string]any
			_ = json.Unmarshal(args[3], &opts)
			fp, _ := opts["fp"].(string)
			seen = append(seen, fp)
			return map[string]any{"done": true, "outcome": browserOutcomeFormChanged}
		},
		"ping": pingOK,
	}
	r, err := newKitRunner(page).fillField(context.Background(), "f1_a", "fp-name", browserPlanValue{Field: "Name", Value: "Ada"}, true, nil)
	if err != nil || r.Outcome != browserOutcomeFormChanged {
		t.Fatalf("%+v %v", r, err)
	}
	if len(seen) != 1 || seen[0] != "fp-name" {
		t.Fatalf("fp sent to the kit: %v", seen)
	}
}
