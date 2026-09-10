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
	value, ok := keys["model.foo=bar.api_key"]
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
