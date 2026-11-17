package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func browserCmd(t *testing.T, params map[string]any) commandMsg {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return commandMsg{Command: browserDeviceCommand, Args: []string{string(b)}}
}

func TestParseBrowserCommandAcceptsEveryOp(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	digest := strings.Repeat("a", 64)
	cases := []map[string]any{
		{"op": "tabs"},
		{"op": "stop"},
		{"op": "open", "url": "https://example.com/a?b=1"},
		{"op": "read", "tabId": "d:ABC123", "includeText": false},
		{"op": "fill", "tabId": "a:42", "values": []map[string]any{{"field": " Email ", "value": "a@b.c"}, {"field": "IBAN", "value": "x", "confirmed": true}}},
		{"op": "click", "tabId": "d:ABC", "ref": "f12_ab3d"},
		{"op": "navigate", "tabId": "d:ABC", "url": "http://example.com"},
		{"op": "navigate", "tabId": "d:ABC", "action": "back"},
		{"op": "screenshot", "tabId": "d:ABC", "fullPage": true},
		{"op": "prepareSubmit", "tabId": "d:ABC"},
		{"op": "prepareSubmit", "tabId": "d:ABC", "ref": "f3_x"},
		{"op": "submit", "tabId": "d:ABC", "submitRef": "f7_x", "digest": digest, "requestId": "req-1"},
	}
	for _, c := range cases {
		c["v"], c["commandId"], c["deadlineMs"] = 1, "cmd-1", now.Add(time.Minute).UnixMilli()
		c["someFutureField"] = map[string]any{"ignored": true} // tolerant reader
		p, id, berr := parseBrowserCommand(browserCmd(t, c), now)
		if berr != nil || id != "cmd-1" || p.Op != c["op"] {
			t.Errorf("%v: %+v %q %v", c["op"], p, id, berr)
		}
	}
	p, _, _ := parseBrowserCommand(browserCmd(t, map[string]any{"v": 1, "commandId": "c", "op": "fill", "tabId": "d:A",
		"values": []map[string]any{{"field": "  Full name ", "value": "Ada"}}}), now)
	if p.Values[0].Field != "Full name" {
		t.Errorf("field not trimmed: %q", p.Values[0].Field)
	}
	if p.DeadlineMs != now.Add(browserCommandTTL).UnixMilli() {
		t.Errorf("missing deadline should default to the command TTL, got %d", p.DeadlineMs)
	}
}

func TestParseBrowserCommandRefusals(t *testing.T) {
	now := time.Now()
	long := strings.Repeat("x", browserMaxValueChars+1)
	many := make([]map[string]any, browserMaxPlanValues+1)
	for i := range many {
		many[i] = map[string]any{"field": "a", "value": "b"}
	}
	cases := []struct {
		name   string
		params map[string]any
		code   string
	}{
		{"version", map[string]any{"v": 2, "commandId": "c", "op": "tabs"}, browserErrCommandInvalid},
		{"unknown op", map[string]any{"v": 1, "commandId": "c", "op": "download"}, browserErrCommandInvalid},
		{"open javascript", map[string]any{"v": 1, "commandId": "c", "op": "open", "url": "javascript:alert(1)"}, browserErrCommandInvalid},
		{"open file", map[string]any{"v": 1, "commandId": "c", "op": "open", "url": "file:///etc/passwd"}, browserErrCommandInvalid},
		{"open creds", map[string]any{"v": 1, "commandId": "c", "op": "open", "url": "https://u:p@example.com"}, browserErrCommandInvalid},
		{"open none", map[string]any{"v": 1, "commandId": "c", "op": "open"}, browserErrCommandInvalid},
		{"read no tab", map[string]any{"v": 1, "commandId": "c", "op": "read"}, browserErrCommandInvalid},
		{"read foreign tab", map[string]any{"v": 1, "commandId": "c", "op": "read", "tabId": "x:1"}, browserErrTabNotFound},
		{"read odd tab", map[string]any{"v": 1, "commandId": "c", "op": "read", "tabId": "d:../etc"}, browserErrTabNotFound},
		{"click no ref", map[string]any{"v": 1, "commandId": "c", "op": "click", "tabId": "d:A"}, browserErrCommandInvalid},
		{"click bad ref", map[string]any{"v": 1, "commandId": "c", "op": "click", "tabId": "d:A", "ref": "a b"}, browserErrCommandInvalid},
		{"navigate both", map[string]any{"v": 1, "commandId": "c", "op": "navigate", "tabId": "d:A", "url": "https://a.com", "action": "back"}, browserErrCommandInvalid},
		{"navigate neither", map[string]any{"v": 1, "commandId": "c", "op": "navigate", "tabId": "d:A"}, browserErrCommandInvalid},
		{"navigate action", map[string]any{"v": 1, "commandId": "c", "op": "navigate", "tabId": "d:A", "action": "home"}, browserErrCommandInvalid},
		{"fill empty", map[string]any{"v": 1, "commandId": "c", "op": "fill", "tabId": "d:A", "values": []any{}}, browserErrCommandInvalid},
		{"fill too many", map[string]any{"v": 1, "commandId": "c", "op": "fill", "tabId": "d:A", "values": many}, browserErrCommandInvalid},
		{"fill long value", map[string]any{"v": 1, "commandId": "c", "op": "fill", "tabId": "d:A", "values": []map[string]any{{"field": "a", "value": long}}}, browserErrCommandInvalid},
		{"fill blank field", map[string]any{"v": 1, "commandId": "c", "op": "fill", "tabId": "d:A", "values": []map[string]any{{"field": "  ", "value": "x"}}}, browserErrCommandInvalid},
		{"submit no digest", map[string]any{"v": 1, "commandId": "c", "op": "submit", "tabId": "d:A", "submitRef": "f1_a", "requestId": "r"}, browserErrCommandInvalid},
		{"submit upper digest", map[string]any{"v": 1, "commandId": "c", "op": "submit", "tabId": "d:A", "submitRef": "f1_a", "requestId": "r", "digest": strings.Repeat("A", 64)}, browserErrCommandInvalid},
		{"wrong type", map[string]any{"v": 1, "commandId": "c", "op": "screenshot", "tabId": "d:A", "fullPage": "yes"}, browserErrCommandInvalid},
	}
	for _, c := range cases {
		_, id, berr := parseBrowserCommand(browserCmd(t, c.params), now)
		if berr == nil || berr.Code != c.code {
			t.Errorf("%s: %v, want %s", c.name, berr, c.code)
		}
		if id != "c" {
			t.Errorf("%s: the refusal must still name the command (got %q)", c.name, id)
		}
	}
	// Without a usable commandId nothing can be reported.
	for _, bad := range []commandMsg{
		{Command: browserDeviceCommand},
		{Command: browserDeviceCommand, Args: []string{"not json"}},
		{Command: browserDeviceCommand, Args: []string{`{"v":1,"commandId":"a/b","op":"tabs"}`}},
		{Command: browserDeviceCommand, Args: []string{`{"v":1,"op":"tabs"}`}},
		{Command: browserDeviceCommand, Args: []string{"{}", "{}"}},
		{Command: browserDeviceCommand, Args: []string{`{"commandId":"c","pad":"` + strings.Repeat("x", browserMaxParamsBytes) + `"}`}},
	} {
		if _, id, berr := parseBrowserCommand(bad, now); id != "" || berr == nil {
			t.Errorf("%.60v: id %q err %v", bad.Args, id, berr)
		}
	}
}

func TestValidateBrowserURL(t *testing.T) {
	ok := map[string]string{
		"https://example.com":           "https://example.com",
		"HTTP://Example.com/path?q=1#x": "HTTP://Example.com/path?q=1#x",
		"  https://a.b/c  ":             "https://a.b/c",
		"https://例え.jp/パス":              "https://例え.jp/パス",
	}
	for in, want := range ok {
		got, err := validateBrowserURL(in)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "example.com", "/relative", "ftp://a.b", "javascript:void(0)", "data:text/html,hi",
		"chrome://settings", "file:///x", "https://", "https://user@a.b", "https://a.b/\nx", "https://a.b/" + strings.Repeat("x", browserMaxURLChars)} {
		if _, err := validateBrowserURL(bad); err == nil {
			t.Errorf("%.60q accepted", bad)
		}
	}
	if jsLength("a😀") != 3 {
		t.Errorf("jsLength counts UTF-16 units like String.length")
	}
}

func TestBrowserTabIDs(t *testing.T) {
	for _, c := range []struct{ route, raw string }{{browserRouteDevTools, "9F2A1B"}, {browserRouteAppleScript, "1234567"}} {
		id := makeBrowserTabID(c.route, c.raw)
		route, raw, ok := parseBrowserTabID(id)
		if !ok || route != c.route || raw != c.raw {
			t.Errorf("%s: %s %s %v", id, route, raw, ok)
		}
	}
	for _, bad := range []string{"", "9F2A", "x:1", "d:", "d:a b", "a:" + strings.Repeat("1", 300)} {
		if _, _, ok := parseBrowserTabID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if !isBrowserHTTPURL("https://a.b/") || isBrowserHTTPURL("chrome://newtab/") || isBrowserHTTPURL("about:blank") {
		t.Error("isBrowserHTTPURL")
	}
}

// stop passes the update drain and the rate limiter; no other browser op does.
func TestBrowserStopIsAlwaysAdmitted(t *testing.T) {
	stop := browserCmd(t, map[string]any{"v": 1, "commandId": "c", "op": "stop"})
	fill := browserCmd(t, map[string]any{"v": 1, "commandId": "c", "op": "fill"})
	if !isBrowserStopCommand(stop) || isBrowserStopCommand(fill) {
		t.Fatal("isBrowserStopCommand")
	}
	if isBrowserStopCommand(commandMsg{Command: "__other__", Args: stop.Args}) {
		t.Fatal("only __browser__ is a browser stop")
	}
	if !isDrainPassThrough(stop) || isDrainPassThrough(fill) {
		t.Fatal("drain: only a browser stop passes")
	}
	if isInternalDemandCommand(browserDeviceCommand) {
		t.Fatal("__browser__ is model-driven: it must face the staleness and rate checks")
	}
}
