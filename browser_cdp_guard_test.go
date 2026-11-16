package main

import "testing"

// The network backstop's decision for one paused document request.
func TestGuardContinues(t *testing.T) {
	cases := []struct {
		name               string
		method             string
		mainFrame, allowGT bool
		url                string
		want               bool
	}{
		{"fill: main-frame link", "GET", true, false, "https://a.example/next", true},
		{"fill: main-frame GET with query", "GET", true, false, "https://a.example/search?q=x", true},
		{"fill: child-frame GET", "GET", false, false, "https://a.example/frame", false},
		{"fill: main-frame POST", "POST", true, false, "https://a.example/submit", false},
		{"click: main-frame link", "GET", true, true, "https://a.example/next?page=2", true},
		{"click: child frame loads plain content", "GET", false, true, "https://a.example/embed/video", true},
		{"click: GET form into a hidden iframe", "GET", false, true, "https://a.example/sneaky?name=Daniel&card=4111", false},
		{"click: child frame with an empty query", "GET", false, true, "https://a.example/sneaky?", false},
		{"click: child-frame POST", "POST", false, true, "https://a.example/sneaky", false},
		{"click: unparseable child-frame URL", "GET", false, true, "https://a.example/%zz?x", false},
	}
	for _, c := range cases {
		if got := guardContinues(c.method, c.mainFrame, c.allowGT, c.url); got != c.want {
			t.Errorf("%s: guardContinues(%s, main=%v, allowGET=%v, %s) = %v, want %v", c.name, c.method, c.mainFrame, c.allowGT, c.url, got, c.want)
		}
	}
}
