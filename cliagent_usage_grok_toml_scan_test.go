package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A QUOTED key segment may carry an `=` of its own. Taking the first `=` in the
// line split `"foo=bar".api_key` as `model.foo`, so the pin never matched
// grokModelCredentialTOMLKey and a run grok's own parser bills by API key was
// attributed to the cached login.
func TestGrokTOMLWalk_FindsAssignmentSeparatorOutsideQuotedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[model]\n\"foo=bar\".api_key = \"xai-quoted-separator-sentinel\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := map[string]string{}
	if complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		keys[key] = value
		return true
	}); !complete {
		t.Fatal("sweep reported incomplete for a config it read to the end")
	}
	// The sweep hands back RE-ENCODED key paths, so a segment that is not a
	// bare key keeps its quotes — that boundary is what stops a quoted
	// `"model.api_key"` from impersonating the two-segment credential.
	value, ok := keys[`model."foo=bar".api_key`]
	if !ok {
		t.Fatalf("keys = %v, want the per-model credential whose model name contains an `=`", keys)
	}
	if !strings.Contains(value, "xai-quoted-separator-sentinel") {
		t.Fatalf("value = %q, want the credential's right-hand side", value)
	}
	if _, ok := keys["model.foo"]; ok {
		t.Fatalf("keys = %v, want no key truncated at an `=` inside a quoted segment", keys)
	}
	if !grokConfigPinsCredential(path) {
		t.Fatal("pinned credential missed: the separator was taken from inside the quoted model name")
	}
}

// A basic multiline string still honours backslash escapes, so `\"""` is a
// literal quote plus two content quotes, never the terminator. Treating it as
// the close resumed configuration parsing inside the string body, where a
// section-shaped line re-scoped every LATER assignment — a root `model.api_key`
// read as `other.model.api_key`, which the attribution guard sees as no pinned
// credential for an API-key-billed run.
func TestGrokTOMLWalk_IgnoresEscapedDelimiterInsideMultilineString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `notes = """
an escaped \""" is body, not the close
[other]
"""
[model]
api_key = "xai-escaped-delimiter-sentinel"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := map[string]string{}
	if complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		keys[key] = value
		return true
	}); !complete {
		t.Fatal("sweep reported incomplete for a config whose multiline string closes")
	}
	if _, ok := keys["model.api_key"]; !ok {
		t.Fatalf("keys = %v, want the root-scoped model.api_key", keys)
	}
	for key := range keys {
		if strings.HasPrefix(key, "other.") {
			t.Fatalf("key %q was scoped under a table that only exists inside a string body", key)
		}
	}
	if !grokConfigPinsCredential(path) {
		t.Fatal("pinned credential missed: an escaped delimiter ended the string body early")
	}
}

// The same escape rule inside a COMPOSITE value: an escaped delimiter in a
// multiline string nested in an array must not end the string, or the array's
// remaining body lines are read as configuration.
func TestGrokTOMLWalk_IgnoresEscapedDelimiterInsideArrayString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `notes = [
"""
an escaped \""" is body
[other]
""",
]
[model]
api_key = "xai-escaped-array-sentinel"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := map[string]string{}
	if complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		keys[key] = value
		return true
	}); !complete {
		t.Fatal("sweep reported incomplete for a config whose array and string both close")
	}
	if _, ok := keys["model.api_key"]; !ok {
		t.Fatalf("keys = %v, want the root-scoped model.api_key", keys)
	}
	for key := range keys {
		if strings.HasPrefix(key, "other.") {
			t.Fatalf("key %q was scoped under a table that only exists inside an array's string body", key)
		}
	}
	if !grokConfigPinsCredential(path) {
		t.Fatal("pinned credential missed: an escaped delimiter inside an array string ended the body early")
	}
}

// A literal (three-single-quote) multiline string defines NO escapes, so a
// backslash before its delimiter is ordinary content and the string still
// closes there. The escape-aware closer must not extend one past its end.
func TestGrokTOMLWalk_LiteralMultilineStringClosesAfterABackslash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `notes = '''
a trailing backslash \'''
[model]
api_key = "xai-literal-close-sentinel"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := map[string]string{}
	if complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		keys[key] = value
		return true
	}); !complete {
		t.Fatal("sweep reported incomplete for a literal multiline string that closes")
	}
	if _, ok := keys["model.api_key"]; !ok {
		t.Fatalf("keys = %v, want the credential that follows a closed literal string", keys)
	}
}

// A multiline string inside a composite can CLOSE mid-line, and TOML permits a
// comment after it. Skipping comment stripping for the whole line just because
// it began in string body let a trailing `# ]` be counted as the composite's
// closing bracket, so the array ended early, its next element line was read as
// a table header, and every LATER root key was re-scoped — a root
// `model.api_key` read as `other.model.api_key`, which the attribution guard
// sees as no pinned credential for a run grok's own parser bills by API key.
func TestGrokTOMLWalk_StripsCommentsAfterAMultilineStringCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `tools = [
  """
body line
""", # ]
  [other]
]
model.api_key = "xai-closing-comment-sentinel"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := map[string]string{}
	if complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		keys[key] = value
		return true
	}); !complete {
		t.Fatal("sweep reported incomplete for a config it read to the end")
	}
	if _, ok := keys["other.model.api_key"]; ok {
		t.Fatalf("keys = %v, want no key re-scoped by a composite that closed on a comment's bracket", keys)
	}
	if _, ok := keys["model.api_key"]; !ok {
		t.Fatalf("keys = %v, want the root credential at its own scope", keys)
	}
	if !grokConfigPinsCredential(path) {
		t.Fatal("pinned credential missed: the trailing comment's `]` closed the composite early")
	}
}

// The comment itself is not value body either: joining it into the logical
// right-hand side would let commented-out text reach the callers that descend
// into inline tables.
func TestGrokTOMLCompositeLineDepth_DropsACommentAfterTheClose(t *testing.T) {
	code, depth, running := grokTOMLCompositeLineDepth(`""", # ]`, `"""`)
	if running != "" {
		t.Fatalf("running = %q, want the string body closed on this line", running)
	}
	if depth != 0 {
		t.Fatalf("depth = %d, want 0 — the bracket lives in a comment", depth)
	}
	if strings.Contains(code, "#") || strings.Contains(code, "]") {
		t.Fatalf("code = %q, want the trailing comment dropped", code)
	}
}

// A `#` BEFORE the closing delimiter is string content, not a comment.
func TestGrokTOMLCompositeLineDepth_KeepsAHashInsideTheStringBody(t *testing.T) {
	code, _, running := grokTOMLCompositeLineDepth(`still # body`, `"""`)
	if running != `"""` {
		t.Fatalf("running = %q, want the string body still open", running)
	}
	if !strings.Contains(code, "# body") {
		t.Fatalf("code = %q, want the `#` kept as string content", code)
	}
}

// A same-line multiline string whose BODY contains an odd number of quote
// characters left the single-quote toggle reading "outside a string" at the
// `#`, so the inline-comment strip took the real closing delimiter with the
// comment. The sweep then read the mutilated line as an unclosed multiline
// opener, ran to EOF with the string still open, and reported the scan
// incomplete — which contests a cached-login run over a config that pins no
// credential at all.
func TestGrokTOMLWalk_TripleQuotedBodyWithAQuoteAndAHashStaysComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `note = """contains " # text""" # trailing
model.api_key = "xai-triple-quote-sentinel"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	keys := map[string]string{}
	if complete := walkGrokTOMLAssignments(path, func(key, value string) bool {
		keys[key] = value
		return true
	}); !complete {
		t.Fatal("sweep reported incomplete for a config whose multiline string closes on its own line")
	}
	if _, ok := keys["model.api_key"]; !ok {
		t.Fatalf("keys = %v, want the assignment after the multiline string", keys)
	}
	if !grokConfigPinsCredential(path) {
		t.Fatal("pinned credential missed: the closing delimiter was stripped as a comment")
	}
}

// The strip must leave a triple-quoted body untouched — every `#` inside one is
// content — while still dropping a comment that follows the closing delimiter.
func TestGrokTOMLStripInlineComment_HonorsTripleQuotedStrings(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want string
	}{
		{
			name: "closed body keeps its hash and loses the trailing comment",
			line: `note = """contains " # text""" # trailing`,
			want: `note = """contains " # text"""`,
		},
		{
			name: "literal triple quotes are content too",
			line: `note = '''a ' # b''' # trailing`,
			want: `note = '''a ' # b'''`,
		},
		{
			name: "an unclosed opener makes the rest of the line body",
			line: `note = """opens # here`,
			want: `note = """opens # here`,
		},
		{
			name: "an ordinary single-quoted value still strips",
			line: `key = "value" # trailing`,
			want: `key = "value"`,
		},
		{
			name: "a hash inside an ordinary string is kept",
			line: `pattern = "Bash(#magic)"`,
			want: `pattern = "Bash(#magic)"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokTOMLStripInlineComment(tc.line); got != tc.want {
				t.Fatalf("grokTOMLStripInlineComment(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}
