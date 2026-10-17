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
// is recorded with the time it was observed and a description of every
// credential source the CLI reads — a one-way fingerprint of META_API_KEY, and
// the size and mtime of each auth.json candidate (never their contents). While
// the refusal is the newest evidence, the usage scan reports Muse Code signed
// out with an error notice; terminal-service turns an error notice into
// `blocked` (annotateBlockedCliAgents), and routing skips a blocked CLI.
//
// Newer positive evidence ends it:
//   - a credential source ADDED or REPLACED since (`muse login` wrote auth.json,
//     the key was set or changed) — a source that was only removed is not a
//     fix;
//   - a Muse Code run completed (`run.terminal.completed`);
//   - a live usage probe got a reading (the card's Refresh, which skips its
//     quota cooldown while signed out) — how a keychain login, which leaves no
//     file to see, clears it.
//
// Evidence is ordered by when it was observed, never by when it was written.
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

const museCodeAuthStateSchemaVersion = 2

// museCodeAuthState is the persisted evidence: the newest refusal, with the
// credential sources as they were then, and the newest proof of a sign-in.
// Muse Code is signed out while the refusal is the NEWER of the two and no
// credential source has been added or replaced since it. Times are when the
// evidence was OBSERVED, so evidence recorded late (a slow stat, a scheduler)
// can never overwrite newer evidence.
type museCodeAuthState struct {
	SchemaVersion        int               `json:"schemaVersion"`
	FailureObservedAtMs  int64             `json:"failureObservedAtMs,omitempty"`
	CredentialSources    map[string]string `json:"credentialSources,omitempty"`
	SignedInObservedAtMs int64             `json:"signedInObservedAtMs,omitempty"`
}

func (s museCodeAuthState) signedOut() bool {
	return s.FailureObservedAtMs > 0 && s.FailureObservedAtMs > s.SignedInObservedAtMs
}

var museCodeAuthStateMu sync.Mutex

// museCodeAuthStatePath: AIEXPEDITE_MUSECODE_AUTH_STATE overrides it (tests).
func museCodeAuthStatePath() string {
	if p := os.Getenv("AIEXPEDITE_MUSECODE_AUTH_STATE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "musecode_auth_state.json")
}

// museCodeCredentialAbsent is a source's value when it holds nothing.
const museCodeCredentialAbsent = "-"

// museCodeCredentialSources describes each credential source Muse Code reads
// without keeping any of them: a one-way fingerprint of the API key (so
// replacing one key with another changes it), and each candidate file's size
// and modification time.
func museCodeCredentialSources(home string, getenv func(string) string) map[string]string {
	sources := map[string]string{"META_API_KEY": museCodeCredentialAbsent}
	if key := strings.TrimSpace(getenv("META_API_KEY")); key != "" {
		sources["META_API_KEY"] = "fp:" + museCodeAPIKeyFingerprint(key)
	}
	for _, path := range museCodeCredentialPaths(home, getenv) {
		sources[path] = museCodeCredentialAbsent
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			sources[path] = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
		}
	}
	return sources
}

// museCodeCredentialAddedOrReplaced: a source holds something now that it did
// not hold at the refusal (a new or rewritten auth.json, a set or replaced
// key). Only that may be a fix. A source that was REMOVED is not one — the
// rejected credential is gone and nothing replaced it.
func museCodeCredentialAddedOrReplaced(atRefusal, now map[string]string) bool {
	for source, value := range now {
		if value != museCodeCredentialAbsent && atRefusal[source] != value {
			return true
		}
	}
	return false
}

func readMuseCodeAuthStateLocked() museCodeAuthState {
	var state museCodeAuthState
	if !readBoundedJSONFile(museCodeAuthStatePath(), &state) || state.SchemaVersion != museCodeAuthStateSchemaVersion {
		return museCodeAuthState{SchemaVersion: museCodeAuthStateSchemaVersion}
	}
	return state
}

// recordMuseCodeSignedOut records a refusal observed at `now`, unless newer
// evidence (a sign-in, or a later refusal) is already recorded.
func recordMuseCodeSignedOut(now time.Time, home string, getenv func(string) string) {
	// Read before the lock (stats can be slow); the decision below compares
	// observation times, so a late write can never displace newer evidence.
	sources := museCodeCredentialSources(home, getenv)
	at := now.UnixMilli()
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	state := readMuseCodeAuthStateLocked()
	if state.SignedInObservedAtMs >= at || state.FailureObservedAtMs >= at {
		return
	}
	state.FailureObservedAtMs = at
	state.CredentialSources = sources
	_ = writeJSONFileAtomic(museCodeAuthStatePath(), state)
}

// recordMuseCodeSignedIn records positive evidence of a sign-in observed at
// `now` (a completed run, a probe reading): it outranks every older refusal.
func recordMuseCodeSignedIn(now time.Time) {
	at := now.UnixMilli()
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	state := readMuseCodeAuthStateLocked()
	if at <= state.SignedInObservedAtMs {
		return
	}
	if state.FailureObservedAtMs == 0 && state.SignedInObservedAtMs == 0 {
		return // nothing to outrank: no file for a CLI that never refused
	}
	state.SignedInObservedAtMs = at
	_ = writeJSONFileAtomic(museCodeAuthStatePath(), state)
}

// museCodeSignedOutMarked reports whether the newest evidence is a refusal
// (the live probe skips its quota cooldown then: Refresh must be able to
// clear it).
func museCodeSignedOutMarked() bool {
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	return readMuseCodeAuthStateLocked().signedOut()
}

// museCodeRefusedSince reports whether Muse Code is signed out on a refusal
// observed after `t`. The usage Refresh's click cooldown lets that click
// through: no probe has had the chance to clear that refusal yet.
func museCodeRefusedSince(t time.Time) bool {
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	state := readMuseCodeAuthStateLocked()
	return state.signedOut() && state.FailureObservedAtMs > t.UnixMilli()
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
		recordMuseCodeSignedIn(now)
	case refused:
		home, _ := os.UserHomeDir()
		recordMuseCodeSignedOut(now, home, os.Getenv)
	}
}

// museCodeSignedOutNotice reports whether Muse Code is known to be signed out
// on this computer, with the notice the card and routing show. A credential
// source added or replaced since the refusal ends it (a new sign-in may have
// fixed it; the next run will tell), recorded as of now so an older refusal
// cannot bring it back.
func museCodeSignedOutNotice(home string, getenv func(string) string) (string, bool) {
	sources := museCodeCredentialSources(home, getenv)
	now := time.Now().UnixMilli()
	museCodeAuthStateMu.Lock()
	defer museCodeAuthStateMu.Unlock()
	state := readMuseCodeAuthStateLocked()
	if !state.signedOut() {
		return "", false
	}
	if museCodeCredentialAddedOrReplaced(state.CredentialSources, sources) {
		// Never earlier than the refusal it ends (a wall clock stepped back
		// would otherwise leave the mark standing).
		state.SignedInObservedAtMs = max(now, state.FailureObservedAtMs)
		_ = writeJSONFileAtomic(museCodeAuthStatePath(), state)
		return "", false
	}
	return "Muse Code is not signed in on this computer (it reported missing credentials), so no work is " +
		"routed to it. Run `muse login` in a terminal on this computer (or set META_API_KEY), then press " +
		"Refresh on this card.", true
}
