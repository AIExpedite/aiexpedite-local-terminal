// cliagent_usage_antigravity_redaction_test.go — the decoded script never
// escapes the classifier.
//
// Classifying a Windows execute now DECODES the command's payload, which is a
// user's prompt and may carry credentials, tokens and file contents. The only
// thing that may leave cliagent_usage_antigravity_command.go is a bool: no log
// line, no cache field, no published usage entry, whether the payload matched
// or not. A leak here would be worse than the staleness this feature fixes —
// the agent log is uploaded with diagnostics and the cache is read by the
// signed usage refresh.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helperSecretScript is a payload of exactly the shape the smoke sends, with
// two distinct secrets: one in an argument, one in the prompt body.
const helperSecretScriptSecret = "sk-live-DO-NOT-LEAK-0987654321"
const helperSecretScriptToken = "ghp-DO-NOT-LEAK-abcdefghij"

func helperSecretScript(program string) string {
	return `Set-Location C:\tmp; & '` + program + `' --api-key ` + helperSecretScriptSecret +
		` -p "deploy with token ` + helperSecretScriptToken + `"`
}

// The matching case: the payload really does spawn agy, so it IS decoded, IS
// classified, and a capture IS armed — and still nothing of it is written
// anywhere.
func TestAntigravityClassify_MatchingPayloadNeverEscapes(t *testing.T) {
	home, cache := helperIsolateAntigravityCapture(t, "20ms")
	base := filepath.Join(home, ".gemini", "antigravity-cli")
	helperStartCaptureServer(t, base, helperQuotaJSON, helperStatusJSON)

	args := []string{"-NoProfile", "-NonInteractive", "-EncodedCommand",
		encodeForPowerShell(helperSecretScript(`C:\t\agy.cmd`))}

	logged := captureStdout(t, func() {
		if !commandRunsAntigravity("powershell.exe", args) {
			t.Error("a wrapped agy payload must classify as a spawn")
		}
		release := armAntigravityCaptureForCommand("windows execute", "powershell.exe", args)
		helperAwaitSnapshot(t, cache, time.Time{}, "a capture under the wrapped payload")
		helperStopCapture(t, release)
	})

	helperAssertNoSecrets(t, "the agent log", logged)

	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	helperAssertNoSecrets(t, "the quota cache", string(raw))

	// The run-completion freshness state is written by the same armed run and
	// is uploaded with diagnostics just like the cache, so it carries the same
	// allowlist: timestamps, counters and a hashed fingerprint only. Asserted
	// on the SERIALIZED bytes, not the struct.
	if freshness, readErr := os.ReadFile(antigravityFreshnessPath()); readErr == nil {
		helperAssertNoSecrets(t, "the freshness state", string(freshness))
		var decodedState map[string]json.RawMessage
		if err := json.Unmarshal(freshness, &decodedState); err != nil {
			t.Fatalf("freshness state is not a JSON object: %v", err)
		}
		allowedState := map[string]bool{
			"schemaVersion": true, "runFloorMs": true, "refreshOwedFloorMs": true,
			"refreshOwedAtMs": true, "lastPaidAtMs": true, "attempts": true,
			"gated": true, "accountFingerprint": true, "outcome": true,
		}
		for key := range decodedState {
			if !allowedState[key] {
				t.Errorf("unexpected persisted freshness field %q", key)
			}
		}
	}

	// The persisted snapshot is still exactly the allowlist after a
	// Windows-wrapped capture — the transport must not widen what is cached.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("cache is not a JSON object: %v", err)
	}
	allowedTop := map[string]bool{
		"schemaVersion": true, "observedAt": true, "accountFingerprint": true,
		"account": true, "plan": true, "buckets": true,
	}
	for key := range decoded {
		if !allowedTop[key] {
			t.Errorf("unexpected persisted field %q after a wrapped capture", key)
		}
	}

	// And the entry the signed refresh publishes carries none of it either.
	usage, ok := antigravityUsageParser{}.Parse(home, detectedCLIAgent{Detected: true}, time.Now())
	if !ok {
		t.Fatal("Parse failed")
	}
	published, err := json.Marshal(usage)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	helperAssertNoSecrets(t, "the published usage entry", string(published))
}

// The non-matching case, which is the more dangerous one: a payload that is NOT
// agy is still fully decoded before the answer comes back "no", so a classifier
// that logged what it rejected would leak every command the device ever runs.
func TestAntigravityClassify_NonMatchingPayloadNeverEscapes(t *testing.T) {
	helperIsolateAntigravityCapture(t, "1h")

	args := []string{"-NoProfile", "-EncodedCommand",
		encodeForPowerShell(helperSecretScript(`C:\t\npm.cmd`))}

	logged := captureStdout(t, func() {
		if commandRunsAntigravity("powershell.exe", args) {
			t.Error("an npm payload must not classify as an agy spawn")
		}
		// Arming is the other half: a no-op release must also be silent.
		armAntigravityCaptureForCommand("windows execute", "powershell.exe", args)()
	})
	helperAssertNoSecrets(t, "the agent log", logged)
	if got := antigravityCaptureArms.Load(); got != 0 {
		t.Errorf("arms=%d, want 0 for a payload that never spawns agy", got)
	}
}

// helperAssertNoSecrets fails with the surface named, so a failure says WHERE
// the payload escaped rather than only that it did.
func helperAssertNoSecrets(t *testing.T, surface, body string) {
	t.Helper()
	for _, secret := range []string{
		helperSecretScriptSecret, helperSecretScriptToken,
		"--api-key", "deploy with token", `C:\tmp`,
	} {
		if strings.Contains(body, secret) {
			t.Errorf("%s leaked %q from the classified payload:\n%s", surface, secret, body)
		}
	}
}
