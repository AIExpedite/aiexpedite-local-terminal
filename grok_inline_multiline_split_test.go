package main

import "testing"

// An inline value the sweep JOINED may carry a multiline (triple-quoted)
// string, and a literal one defines no escapes — so an ordinary apostrophe or
// quote inside its body is just text. Reading that byte as the terminator split
// the body mid-string, left the following `api_key` inside a falsely open quote
// and named the cached login above a record the pinned key was billed for.
func TestGrokDirectRunBillingIdentity_ContestsACredentialAfterAMultilineString(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{
			"literal multiline holds a quote and a comma",
			"version_overrides = [{ note = '''it's, text''', model = { api_key = \"xai-inline-sentinel\" } }]\n",
			grokContestedBillingIdentity,
		},
		{
			"basic multiline holds a quote and a comma",
			"model = { note = \"\"\"say \"hi\", ok\"\"\", api_key = \"xai-inline-sentinel\" }\n",
			grokContestedBillingIdentity,
		},
		// The mirror case: credential-SHAPED text quoted inside a multiline
		// note is not a pin, and must not cost an honest run its observability.
		{
			"credential shaped text inside a multiline note",
			"model = { note = '''api_key = \"xai-not-a-pin\"''', temperature = 0.2 }\n",
			"acct-login",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetGrokBillingAttribution(t)
			helperNoGrokSystemConfigLayers(t)
			base := helperGrokHomeWithAccount(t, "acct-login")
			repo := t.TempDir()
			helperWriteGrokProjectConfig(t, repo, tc.config)
			if got := grokDirectRunBillingIdentity(grokDirectRunLaunch{Cwd: repo}, base); got != tc.want {
				t.Fatalf("identity = %q, want %q for config %q", got, tc.want, tc.config)
			}
		})
	}
}

// splitGrokInlineTableFields must split on the top-level commas grok's own
// parser sees. A literal multiline string defines no escapes, so an apostrophe
// in its body is text, not a terminator; treating it as one moved the field
// boundary into the string and left the assignment after it half-quoted. The
// second result reports whether every string and bracket closed — a body that
// ends inside one cannot be split faithfully at all, and the caller fails
// CLOSED on it rather than trusting the fragments.
func TestSplitGrokInlineTableFields_TracksMultilineStrings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		want   []string
		wantOK bool
	}{
		{"plain fields", `a = 1, b = 2`, []string{`a = 1`, ` b = 2`}, true},
		{
			"comma and quote inside a literal multiline",
			`note = '''it's, text''', api_key = "x"`,
			[]string{`note = '''it's, text'''`, ` api_key = "x"`},
			true,
		},
		{
			"comma and quote inside a basic multiline",
			`note = """say "hi", ok""", api_key = "x"`,
			[]string{`note = """say "hi", ok"""`, ` api_key = "x"`},
			true,
		},
		{"unterminated multiline", `note = '''never closes, api_key = "x"`, nil, false},
		{"unterminated quote", `note = "never closes, api_key = x`, nil, false},
		{"unterminated table", `model = { api_key = "x"`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields, ok := splitGrokInlineTableFields(tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v for %q", ok, tc.wantOK, tc.body)
			}
			if !ok {
				return
			}
			if len(fields) != len(tc.want) {
				t.Fatalf("fields = %q, want %q", fields, tc.want)
			}
			for i := range fields {
				if fields[i] != tc.want[i] {
					t.Fatalf("field %d = %q, want %q", i, fields[i], tc.want[i])
				}
			}
		})
	}
}
