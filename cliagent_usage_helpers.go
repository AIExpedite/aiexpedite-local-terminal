// cliagent_usage_helpers.go — shared helpers used by the per-provider
// usage parsers. Pulled out so each provider file stays focused on its
// own quirks rather than reimplementing the same json/fingerprint logic.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// unmarshalJSON wraps json.Unmarshal so parsers can call it without an
// `error` check at every site. Failures are downgraded to false — the
// caller treats "couldn't decode" the same as "file missing".
func unmarshalJSON(data []byte, into any) bool {
	if err := json.Unmarshal(data, into); err != nil {
		return false
	}
	return true
}

func parseJWTClaims(idToken string, into any) bool {
	if idToken == "" || into == nil {
		return false
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return false
	}
	return unmarshalJSON(payload, into)
}

// fingerprintAccount produces a stable per-account opaque identifier from a
// provider-specific account string (email, OAuth subject, etc.). It is one-way
// (the orchestration layer needs to dedup the SAME account across devices,
// but does NOT need to see the underlying email).
//
// We hash provider name into the input so two different providers happening
// to use the same email don't collide in the deduplication map.
func fingerprintAccount(provider, account string) string {
	if account == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(provider + "\x00" + account))
	// 12 bytes (24 hex) is plenty of collision resistance for a per-workspace
	// set; the full 32 bytes would just bloat the Firestore doc.
	return hex.EncodeToString(sum[:12])
}

// fallbackUnknownAccountFingerprint gives missing-credential snapshots a stable
// per-device bucket without pretending we know the underlying account.
func fallbackUnknownAccountFingerprint(provider, host string, detected detectedCLIAgent) string {
	parts := make([]string, 0, 4)
	for _, value := range []string{host, detected.Path, detected.Version, detected.Name} {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return fingerprintAccount(provider+":unknown", strings.Join(parts, "\x00"))
}

// cliAgentUsageRegistry returns the active parsers in deterministic order.
// Order matters for byte-stable Firestore payloads — see gatherCLIAgentUsage.
func cliAgentUsageRegistry() []cliAgentUsageParser {
	return []cliAgentUsageParser{
		&claudeCodeUsageParser{},
		&codexUsageParser{},
		&antigravityUsageParser{},
		&grokUsageParser{},
	}
}

func cliAgentUsageParserIndex() map[string]cliAgentUsageParser {
	parsers := cliAgentUsageRegistry()
	out := make(map[string]cliAgentUsageParser, len(parsers))
	for _, parser := range parsers {
		out[parser.Provider()] = parser
	}
	return out
}
