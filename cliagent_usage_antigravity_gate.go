// cliagent_usage_antigravity_gate.go — remembers that the installed `agy`
// build refuses loopback quota reads, so the agent stops paying for readings
// it cannot take and the card says why the pool is not refreshing.
//
// Antigravity CLI 1.2.2 (first seen 2026-09-11) put a CSRF interceptor in front
// of every language-server RPC. A request without the per-run
// `x-codeium-csrf-token` header is answered
//
//	401 {"code":"unauthenticated","message":"missing CSRF token"}
//
// on the plain-HTTP and the TLS port alike, before and after the server has
// authenticated its account. The token is generated per run and handed only to
// the CLI's own tool subprocesses. Print mode (`agy -p`, the only headless
// mode) runs no hooks and passes MCP children no token, an env-supplied
// ANTIGRAVITY_CSRF_TOKEN is not adopted, and the value is composed per spawn
// rather than kept in the process environment — established 2026-09-15 on a
// 1.2.3 install, which is also the day the last reading on that machine (taken
// under 1.2.1) turned 84 hours old.
//
// So the working theory of cliagent_usage_antigravity_quota.go — read the
// server while it is up — stopped being true with that build, and every path
// that relied on it paid for nothing: a Refresh click spent a model turn plus
// its 20 s budget to time out, and every real run scanned the CLI's logs for up
// to 15 min to collect 401s.
//
// The marker below is what those paths consult. It is per build (an `agy`
// update is tried again the first time it is seen), it expires after
// antigravityQuotaGateRecheck so a same-version fix or a mistaken marker heals
// on its own, and a successful reading clears it. The last good snapshot stays
// in the cache with its true observedAt; the parser adds a card notice naming
// the build and that reading's age instead of leaving the bars striped with no
// explanation.
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

// antigravityRPCOutcome classifies one loopback RPC.
type antigravityRPCOutcome int

const (
	antigravityRPCOK antigravityRPCOutcome = iota
	// The server answered, and refused for want of its CSRF token. Distinct
	// from a failure because it is a property of the BUILD, not of this
	// attempt: retrying, rescanning logs or waiting will not change it.
	antigravityRPCGated
	// No answer, a non-200 that is not the CSRF refusal, or an undecodable
	// body — the usual "no live server on this port".
	antigravityRPCFailed
)

// antigravityFetchOutcome classifies one quota read on a port.
type antigravityFetchOutcome int

const (
	antigravityFetchOK antigravityFetchOutcome = iota
	antigravityFetchGated
	antigravityFetchFailed
)

// antigravityRefusalIsCSRF reports whether a 401 body is the CSRF interceptor's
// refusal. The code is what the interceptor sets; the message check is a
// second spelling in case a later build keeps the message and renames the
// code. Any other 401 stays a plain failure.
func antigravityRefusalIsCSRF(body []byte) bool {
	var refusal struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &refusal) != nil {
		return false
	}
	return refusal.Code == "unauthenticated" ||
		strings.Contains(strings.ToLower(refusal.Message), "csrf")
}

const (
	// antigravityQuotaGateRecheck is how long a marker suppresses live reads
	// of the same build before one is tried again.
	antigravityQuotaGateRecheck = 24 * time.Hour
	// antigravityQuotaGateEnv relocates the marker (tests isolate from the
	// real machine; mirrors AIEXPEDITE_AGY_QUOTA_CACHE).
	antigravityQuotaGateEnv         = "AIEXPEDITE_AGY_QUOTA_GATE"
	antigravityQuotaGateSchema      = 1
	antigravityQuotaGateMaxVersion  = 64
	antigravityQuotaGateNoticeLimit = 256
)

// antigravityQuotaGate is the persisted marker. Version is the `agy` build that
// refused ("" when the refusal was seen where the build is not known — the
// run-scoped poller); ObservedAt is when.
type antigravityQuotaGate struct {
	SchemaVersion int    `json:"schemaVersion,omitempty"`
	Version       string `json:"version,omitempty"`
	ObservedAt    string `json:"observedAt"`
}

var antigravityQuotaGateMu sync.Mutex

func antigravityQuotaGatePath() string {
	if p := os.Getenv(antigravityQuotaGateEnv); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "antigravity_quota_gate.json")
}

// noteAntigravityQuotaGate records that the build refused. A known version
// never gets overwritten by an unknown one: the poller's "" must not erase the
// build the live probe identified.
func noteAntigravityQuotaGate(version string, now time.Time) {
	version = clampASCII(version, antigravityQuotaGateMaxVersion)
	antigravityQuotaGateMu.Lock()
	defer antigravityQuotaGateMu.Unlock()
	var prev antigravityQuotaGate
	if readJSONFile(antigravityQuotaGatePath(), &prev) && version == "" {
		version = prev.Version
	}
	gate := antigravityQuotaGate{
		SchemaVersion: antigravityQuotaGateSchema,
		Version:       version,
		ObservedAt:    now.UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(gate)
	if err != nil {
		return
	}
	path := antigravityQuotaGatePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, body, 0o600)
}

// clearAntigravityQuotaGate forgets the marker — called when a reading lands.
func clearAntigravityQuotaGate() {
	antigravityQuotaGateMu.Lock()
	defer antigravityQuotaGateMu.Unlock()
	_ = os.Remove(antigravityQuotaGatePath())
}

// antigravityQuotaGateFor returns the marker when it applies to the given
// build now: same version (an unknown version on either side matches, since
// the refusal was observed on whatever is installed), and within the recheck
// window. Everything else — a newer build, an expired marker, no marker — is
// "try it".
func antigravityQuotaGateFor(version string, now time.Time) (antigravityQuotaGate, bool) {
	antigravityQuotaGateMu.Lock()
	defer antigravityQuotaGateMu.Unlock()
	var gate antigravityQuotaGate
	if !readJSONFile(antigravityQuotaGatePath(), &gate) {
		return antigravityQuotaGate{}, false
	}
	observed, err := time.Parse(time.RFC3339, gate.ObservedAt)
	if err != nil || now.Sub(observed) > antigravityQuotaGateRecheck || now.Before(observed.Add(-time.Hour)) {
		return antigravityQuotaGate{}, false
	}
	if gate.Version != "" && version != "" && gate.Version != version {
		return antigravityQuotaGate{}, false
	}
	return gate, true
}

// antigravityGateNotice is the card banner for a gated build. lastObservedAt
// is the cached reading's observation time ("" when there is none).
func antigravityGateNotice(version, lastObservedAt string) string {
	build := "Antigravity CLI"
	if version != "" {
		build += " " + version
	}
	notice := build + " no longer lets this computer read its quota: its language server now requires a per-run token it shares only with its own tools."
	if t, err := time.Parse(time.RFC3339, lastObservedAt); err == nil {
		notice += fmt.Sprintf(" Showing the last reading, taken %s.", t.UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		notice += " No reading has been possible since."
	}
	return clampASCII(notice, antigravityQuotaGateNoticeLimit)
}

// clampASCII bounds a string that is about to be persisted or published,
// keeping it printable.
func clampASCII(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}
