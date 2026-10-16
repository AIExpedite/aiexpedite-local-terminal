// musecode_auth_signal.go — Muse Code says when it is NOT signed in; remember
// it, so work is never routed to it until it is signed in again.
//
// The usage parser (cliagent_usage_musecode.go) fails open on purpose: Muse
// Code 1.4 can keep its login in the OS keychain, which leaves no file for this
// agent to find, so a missing `auth.json` reads "unknown", never "signed out".
// But "unknown" is not blocked, and a Muse Code that is NOT signed in kept
// being routed: prod 2026-10-15/16, every adversarial review seated on AIX5's
// Muse Code ended at kickoff with
//
//	missing meta credentials: run `muse login` or set META_API_KEY, or save
//	credentials at C:\Users\…\.config\muse\auth.json
//
// and the gate then stopped each fix for a person (REVIEWER_OBJECTED).
//
// The CLI's own refusal is POSITIVE evidence, which is what the parser waits
// for. Whenever a Muse Code run prints it (a Ship session or a Chat turn), it
// is recorded together with a stamp of every credential source the CLI reads —
// META_API_KEY present or not, and the size and mtime of each auth.json
// candidate (never their contents). While the stamp is unchanged and nothing
// has proved a sign-in since, the usage scan reports Muse Code signed out with
// an error notice; terminal-service turns an error notice into `blocked`
// (annotateBlockedCliAgents), and routing skips a blocked CLI.
//
// The mark clears on positive evidence only:
//   - a credential source changed (`muse login` wrote auth.json, or the key was
//     set) — the next scan sees a different stamp;
//   - a Muse Code run completed (`run.terminal.completed`);
//   - a live usage probe got a reading (the card's Refresh), which is how a
//     keychain login — no file to stamp — clears it.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// museCodeAuthFailureMarkers are lower-cased substrings of Muse Code's own
// "you are not signed in" refusal. Deliberately narrow: an ordinary failure
// must never take a working CLI out of rotation.
var museCodeAuthFailureMarkers = []string{
	"missing meta credentials",
	"run `muse login`",
}

const museCodeAuthStateSchemaVersion = 1

// museCodeAuthState is the persisted mark: when the refusal was seen and the
// credential sources as they were then.
type museCodeAuthState struct {
	SchemaVersion       int    `json:"schemaVersion"`
	FailureObservedAtMs int64  `json:"failureObservedAtMs"`
	CredentialStamp     string `json:"credentialStamp"`
}

var museCodeAuthStateMu sync.Mutex

// museCodeAuthStatePath: AIEXPEDITE_MUSECODE_AUTH_STATE overrides it (tests).
func museCodeAuthStatePath() string {
	if p := os.Getenv("AIEXPEDITE_MUSECODE_AUTH_STATE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "musecode_auth_state.json")
}

// museCodeCredentialStamp describes the credential sources Muse Code reads
// without reading any of them: the API key's presence, and each candidate
// file's size and modification time ("-" when absent).
func museCodeCredentialStamp(home string, getenv func(string) string) string {
	parts := []string{}
	if strings.TrimSpace(getenv("META_API_KEY")) != "" {
		parts = append(parts, "key:set")
	} else {
		parts = append(parts, "key:-")
	}
	for _, path := range museCodeCredentialPaths(home, getenv) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", path, info.Size(), info.ModTime().UnixNano()))
		} else {
			parts = append(parts, path+":-")
		}
	}
	return strings.Join(parts, "|")
}

func readMuseCodeAuthStateLocked() (museCodeAuthState, bool) {
	var state museCodeAuthState
	if !readBoundedJSONFile(museCodeAuthStatePath(), &state) ||
		state.SchemaVersion != museCodeAuthStateSchemaVersion || state.FailureObservedAtMs <= 0 {
		return museCodeAuthState{}, false
	}
	return state, true
}

// recordMuseCodeSignedOut marks Muse Code as not signed in on this computer.
func recordMuseCodeSignedOut(now time.Time, home string, getenv func(string) string) {
	state := museCodeAuthState{
		SchemaVersion:       museCodeAuthStateSchemaVersion,
		FailureObservedAtMs: now.UnixMilli(),
		CredentialStamp:     museCodeCredentialStamp(home, getenv),
	}
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	_ = writeJSONFileAtomic(museCodeAuthStatePath(), state)
}

// recordMuseCodeSignedIn clears the mark (positive evidence of a sign-in).
func recordMuseCodeSignedIn() {
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	_ = os.Remove(museCodeAuthStatePath())
}

// museCodeAuthEvidence classifies one line of Muse Code output: -1 the CLI
// refused for missing credentials, +1 a run completed (signed in), 0 neither.
//
// Only the CLI's OWN words count. A JSON event carries the model's text
// (`run.output.delta`, `run.terminal.completed`), which can quote anything —
// a review of this very file mentions "missing meta credentials" — so inside
// an event only a FAILED run's reason is read; a plain (non-JSON) line is the
// CLI itself talking (its startup refusal goes to stderr).
func museCodeAuthEvidence(line string) int {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return 0
	}
	if strings.HasPrefix(trimmed, "{") {
		var env museCodeEnvelope
		if json.Unmarshal([]byte(trimmed), &env) != nil {
			return 0
		}
		switch env.PayloadType {
		case "run.terminal.completed":
			return 1
		case "run.terminal.failed":
			if museCodeIsAuthRefusal(env.Payload.ErrorKind + " " + env.Payload.Reason + " " + env.Payload.Message) {
				return -1
			}
		}
		return 0
	}
	if museCodeIsAuthRefusal(trimmed) {
		return -1
	}
	return 0
}

func museCodeIsAuthRefusal(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range museCodeAuthFailureMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// observeMuseCodeAuthOutput reads Muse Code output — one line of a session,
// or a Chat turn's stdout and stderr — and records what it proves. A
// completed run anywhere in it wins over a refusal (it reached the model).
func observeMuseCodeAuthOutput(text string, now time.Time) {
	refused, completed := false, false
	for _, line := range strings.Split(text, "\n") {
		switch museCodeAuthEvidence(line) {
		case -1:
			refused = true
		case 1:
			completed = true
		}
	}
	switch {
	case completed:
		recordMuseCodeSignedIn()
	case refused:
		home, _ := os.UserHomeDir()
		recordMuseCodeSignedOut(now, home, os.Getenv)
	}
}

// museCodeSignedOutNotice reports whether Muse Code is known to be signed out
// on this computer, with the notice the card and routing show. A credential
// source that changed since the refusal clears the mark (a new sign-in may
// have fixed it; the next run will tell).
func museCodeSignedOutNotice(home string, getenv func(string) string) (string, bool) {
	museCodeAuthStateMu.Lock()
	state, ok := readMuseCodeAuthStateLocked()
	museCodeAuthStateMu.Unlock()
	if !ok {
		return "", false
	}
	if state.CredentialStamp != museCodeCredentialStamp(home, getenv) {
		recordMuseCodeSignedIn()
		return "", false
	}
	return "Muse Code is not signed in on this computer (it reported missing credentials), so no work is " +
		"routed to it. Run `muse login` in a terminal on this computer (or set META_API_KEY), then press " +
		"Refresh on this card.", true
}
