package main

import "testing"

// TOML lets up to two extra quotes sit against a multiline string's closing
// delimiter: in `"""foo""""` the content is `foo"` and the LAST three quotes
// terminate it. Reporting only the first triple left the extra quote for the
// scanners to resume ON, which opened a fresh single-quoted string; the scan
// then ended with quote state open and reported itself INCOMPLETE, and
// grokConfigPinsCredential reads an incomplete scan as a pin — contesting a
// direct run over a config that pins no credential at all.
func TestGrokTOMLMultilineCloserSpan_ConsumesExtraQuotes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		s       string
		delim   string
		wantIdx int
		wantLen int
	}{
		{"bare terminator", `foo"""`, `"""`, 3, 3},
		{"one extra quote", `foo""""`, `"""`, 3, 4},
		{"two extra quotes", `foo"""""`, `"""`, 3, 5},
		{"literal one extra quote", "foo''''", "'''", 3, 4},
		{"literal two extra quotes", "foo'''''", "'''", 3, 5},
		// Six in a row is a terminator immediately followed by a new opener,
		// not one long terminator; consuming the run whole would swallow the
		// next string's delimiter and read its body as value syntax.
		{"six quotes close then reopen", `foo""""""bar"""`, `"""`, 3, 3},
		// A basic multiline still honours backslash escapes, so an escaped
		// quote against the run is content and cannot start the terminator.
		{"escaped quote before terminator", `foo\""""`, `"""`, 5, 3},
		{"never closes", `foo"" bar`, `"""`, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, length := grokTOMLMultilineCloserSpan(tc.s, tc.delim)
			if idx != tc.wantIdx || length != tc.wantLen {
				t.Fatalf("span(%q, %q) = (%d, %d), want (%d, %d)", tc.s, tc.delim, idx, length, tc.wantIdx, tc.wantLen)
			}
			if got := grokTOMLMultilineCloserIndex(tc.s, tc.delim); got != tc.wantIdx {
				t.Fatalf("index(%q, %q) = %d, want %d", tc.s, tc.delim, got, tc.wantIdx)
			}
		})
	}
}

// The inline-table split must resume AFTER the whole closing run, or the
// leftover quote leaves the field scan half-quoted and it fails closed on a
// body it could have read faithfully.
func TestSplitGrokInlineTableFields_ClosingRunWithExtraQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			"basic multiline ends with a literal quote",
			`note = """foo"""", api_key = "x"`,
			[]string{`note = """foo""""`, ` api_key = "x"`},
		},
		{
			"literal multiline ends with two literal quotes",
			"note = '''foo''''', api_key = \"x\"",
			[]string{"note = '''foo'''''", ` api_key = "x"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields, ok := splitGrokInlineTableFields(tc.body)
			if !ok {
				t.Fatalf("split(%q) reported an incomplete scan", tc.body)
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

// Same boundary in the comment stripper: resuming on the leftover quote made
// the rest of the line read as string body, so a real trailing comment survived
// into the value.
func TestGrokTOMLStripInlineComment_ClosingRunWithExtraQuotes(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{"basic", `note = """foo"""" # trailing`, `note = """foo""""`},
		{"literal", "note = '''foo''''' # trailing", "note = '''foo'''''"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokTOMLStripInlineComment(tc.line); got != tc.want {
				t.Fatalf("strip(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// End to end: a credential-free config whose multiline value ends with a
// literal quote must leave a direct run attributable to the signed-in account,
// not contested.
func TestGrokDirectRunBillingIdentity_ClosingRunWithExtraQuotesIsNotAPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   string
	}{
		{
			"credential free multiline ending in a quote",
			"model = { note = \"\"\"foo\"\"\"\", temperature = 0.2 }\n",
			"acct-login",
		},
		// The pin on the other side of that run is still seen.
		{
			"credential after a multiline ending in a quote",
			"model = { note = \"\"\"foo\"\"\"\", api_key = \"xai-inline-sentinel\" }\n",
			grokContestedBillingIdentity,
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
