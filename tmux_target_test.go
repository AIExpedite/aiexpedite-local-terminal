// Tests for tmux target resolution (tmux.go). The per-channel session names
// introduced by this change only isolate channels if every has/attach/kill
// addresses its session EXACTLY: prod's `agent` is a prefix of `agent-dev`,
// and tmux falls back to unique-prefix matching, so a bare target would let
// prod attach to — and on shutdown kill — the dev channel's session.
package main

import (
	"os"
	"strings"
	"testing"
)

func TestTmuxTargetForcesExactMatching(t *testing.T) {
	original := tmuxSessionName
	t.Cleanup(func() { tmuxSessionName = original })

	tmuxSessionName = "agent"
	if got := tmuxTarget(); got != "=agent" {
		t.Fatalf("tmuxTarget() = %q, want =agent", got)
	}

	tmuxSessionName = tmuxSessionNameFor("dev")
	if got := tmuxTarget(); got != "="+tmuxSessionNameFor("dev") {
		t.Fatalf("tmuxTarget() = %q, want =%s", got, tmuxSessionNameFor("dev"))
	}
}

// Every channel's target must be unambiguous against every other channel's
// SESSION NAME, which is what tmux's prefix fallback would otherwise match.
func TestTmuxTargetsAreNotPrefixesOfOtherChannels(t *testing.T) {
	original := tmuxSessionName
	t.Cleanup(func() { tmuxSessionName = original })

	for env := range localDefaultsByEnv {
		tmuxSessionName = tmuxSessionNameFor(env)
		target := tmuxTarget()
		if !strings.HasPrefix(target, "=") {
			t.Fatalf("%s target %q is not an exact target", env, target)
		}
		for other := range localDefaultsByEnv {
			name := tmuxSessionNameFor(other)
			if other != env && strings.TrimPrefix(target, "=") == name {
				t.Fatalf("%s and %s resolve to the same session %q", env, other, name)
			}
		}
	}
}

// The bare session name must survive for CREATION: `new-session -s` takes a
// literal name, so passing the `=` target there would create a session
// literally called "=agent" that no target ever finds again.
func TestTmuxSourcesUseExactTargetsButLiteralCreateName(t *testing.T) {
	for _, file := range []string{"tmux.go", "agent.go", "shutdown.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(src), `"-t", tmuxSessionName`) {
			t.Errorf("%s passes a prefix-matchable target; use tmuxTarget()", file)
		}
		if strings.Contains(string(src), `"-s", tmuxTarget()`) {
			t.Errorf("%s creates a session named after an exact target", file)
		}
	}
}
