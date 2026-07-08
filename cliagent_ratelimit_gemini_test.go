// cliagent_ratelimit_gemini_test.go — fixtures for the Gemini quota /
// rate-limit line capture, mirroring the Grok limit-state tests (frame
// classification) plus write/load behaviour of the on-disk cache (downgrade
// guard, TTL expiry, account pinning).

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGeminiLimitStateFromFrame_Reached(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// REST error body shape the CLI surfaces on a hard daily-quota hit.
	frame := map[string]interface{}{
		"error": map[string]interface{}{
			"code":    float64(429),
			"message": "Quota exceeded for quota metric 'Gemini Pro Requests' and limit 'requests per day'",
			"status":  "RESOURCE_EXHAUSTED",
		},
	}
	st, ok := geminiLimitStateFromFrame(frame, now)
	if !ok || st.Severity != geminiLimitReached {
		t.Fatalf("want reached, got ok=%v sev=%q", ok, st.Severity)
	}
	if !strings.Contains(st.Message, "Quota exceeded") {
		t.Fatalf("message not captured: %+v", st)
	}
}

func TestGeminiLimitStateFromFrame_ReasonRateLimitExceeded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// JSON-API error detail shape (`reason: rateLimitExceeded`).
	frame := map[string]interface{}{
		"error": map[string]interface{}{
			"errors": []interface{}{
				map[string]interface{}{"reason": "rateLimitExceeded", "domain": "global"},
			},
		},
	}
	st, ok := geminiLimitStateFromFrame(frame, now)
	if !ok || st.Severity != geminiLimitReached {
		t.Fatalf("want reached from rateLimitExceeded reason, got ok=%v sev=%q", ok, st.Severity)
	}
}

func TestGeminiLimitStateFromFrame_Approaching(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"event": "quota_approaching",
	}
	st, ok := geminiLimitStateFromFrame(frame, now)
	if !ok || st.Severity != geminiLimitApproaching {
		t.Fatalf("want approaching, got ok=%v sev=%q", ok, st.Severity)
	}
}

func TestGeminiLimitStateFromFrame_NoSignal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if _, ok := geminiLimitStateFromFrame(map[string]interface{}{"type": "text", "text": "hi"}, now); ok {
		t.Fatal("ordinary frame must not produce a limit state")
	}
}

// Gemini ACP session-update frames can carry the limit signal under
// `params.update.sessionUpdate` (same envelope Grok uses); the walker must
// recognise the sessionUpdate key or a real quota session-update passes the
// prefilter but returns ok=false.
func TestGeminiLimitStateFromFrame_SessionUpdate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"params": map[string]interface{}{
			"update": map[string]interface{}{
				"sessionUpdate": "quota_exceeded",
			},
		},
	}
	st, ok := geminiLimitStateFromFrame(frame, now)
	if !ok || st.Severity != geminiLimitReached {
		t.Fatalf("want reached from sessionUpdate, got ok=%v sev=%q", ok, st.Severity)
	}
}

// Ordinary chat text that merely TALKS ABOUT quota limits (an assistant
// explaining an API error, a user pasting one) rides in the same
// `content`/`text` keys as real notices. It must not classify — a false
// "reached" would pin an error banner on the CLI Agents card for the 6h TTL.
func TestGeminiLimitStateFromFrame_ChatProseIsNotALimitSignal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frames := []map[string]interface{}{
		// ACP agent_message_chunk: assistant prose mentioning a quota error.
		{
			"method": "session/update",
			"params": map[string]interface{}{
				"update": map[string]interface{}{
					"sessionUpdate": "agent_message_chunk",
					"content": map[string]interface{}{
						"type": "text",
						"text": "Your API call failed because the quota limit was hit; quota exceeded errors mean you should retry tomorrow.",
					},
				},
			},
		},
		// User prompt text quoting a rate-limit error.
		{
			"prompt": []interface{}{
				map[string]interface{}{"type": "text", "text": "why did I get 'rate limit exceeded' from my script?"},
			},
		},
	}
	for i, frame := range frames {
		if _, ok := geminiLimitStateFromFrame(frame, now); ok {
			t.Errorf("frame %d: chat prose must not produce a limit state", i)
		}
	}
}

// Prose inside an error-declaring envelope still classifies: the CLI's error
// frames can carry the quota text under `content` rather than `message`.
func TestGeminiLimitStateFromFrame_ErrorEnvelopeContentStillClassifies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	frame := map[string]interface{}{
		"type":    "error",
		"content": "Quota exceeded for quota metric 'Gemini Pro Requests'",
	}
	st, ok := geminiLimitStateFromFrame(frame, now)
	if !ok || st.Severity != geminiLimitReached {
		t.Fatalf("want reached from error-enveloped content, got ok=%v sev=%q", ok, st.Severity)
	}
	if !strings.Contains(st.Message, "Quota exceeded") {
		t.Fatalf("message not captured: %+v", st)
	}
}

// TestCaptureGeminiUsageLimitLine_WritesCache drives the full line-capture
// path: prefilter → decode → classify → cache write, against an isolated
// cache location.
func TestCaptureGeminiUsageLimitLine_WritesCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "gemini_usage_limit.json")
	t.Setenv("AIEXPEDITE_GEMINI_LIMIT_CACHE", cachePath)

	captureGeminiUsageLimitLine(
		`{"error":{"code":429,"message":"Quota exceeded for quota metric 'Gemini Pro Requests'","status":"RESOURCE_EXHAUSTED"}}`,
		time.Now(),
	)

	var state geminiUsageLimitState
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache was never written: %v", err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("cache is not valid JSON: %v\n%s", err, raw)
	}
	if state.Severity != geminiLimitReached {
		t.Errorf("expected severity reached, got %q", state.Severity)
	}
	if !strings.Contains(state.Message, "Quota exceeded") {
		t.Errorf("expected quota message captured, got %q", state.Message)
	}
}

// TestCaptureGeminiUsageLimitLine_IgnoresOrdinaryLines pins the prefilter +
// classifier negative path: ordinary output must never create the cache file.
func TestCaptureGeminiUsageLimitLine_IgnoresOrdinaryLines(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "gemini_usage_limit.json")
	t.Setenv("AIEXPEDITE_GEMINI_LIMIT_CACHE", cachePath)

	captureGeminiUsageLimitLine(`{"type":"message","role":"assistant","content":"hello"}`, time.Now())
	captureGeminiUsageLimitLine(`not json at all`, time.Now())
	// Passes the cheap prefilter (mentions "quota") but is an ordinary ACP
	// chat chunk — the classifier must reject it end-to-end.
	captureGeminiUsageLimitLine(`{"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"That 429 means your quota limit was exceeded upstream."}}}}`, time.Now())

	if _, err := os.ReadFile(cachePath); err == nil {
		t.Fatal("cache must not be written for ordinary lines")
	}
}

// TestWriteGeminiUsageLimitState_ApproachingDoesNotDowngradeReached pins the
// downgrade guard: while a reached state is still live, a fresh approaching
// signal for the same account only refreshes observedAt.
func TestWriteGeminiUsageLimitState_ApproachingDoesNotDowngradeReached(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "gemini_usage_limit.json")
	t.Setenv("AIEXPEDITE_GEMINI_LIMIT_CACHE", cachePath)

	base := time.Unix(1_700_000_000, 0)
	writeGeminiUsageLimitState(cachePath, geminiUsageLimitState{
		Severity:     geminiLimitReached,
		Message:      "Quota exceeded",
		ObservedAt:   base.UTC().Format(time.RFC3339),
		ObservedAtMs: base.UnixMilli(),
	}, "fp-1")

	later := base.Add(10 * time.Minute)
	writeGeminiUsageLimitState(cachePath, geminiUsageLimitState{
		Severity:     geminiLimitApproaching,
		ObservedAt:   later.UTC().Format(time.RFC3339),
		ObservedAtMs: later.UnixMilli(),
	}, "fp-1")

	raw, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("cache missing: %v", err)
	}
	var state geminiUsageLimitState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("cache is not valid JSON: %v", err)
	}
	if state.Severity != geminiLimitReached {
		t.Errorf("approaching must not downgrade a live reached state; got %q", state.Severity)
	}
	if state.ObservedAtMs != later.UnixMilli() {
		t.Errorf("expected observedAt refreshed to %d, got %d", later.UnixMilli(), state.ObservedAtMs)
	}

	// A different account discards the prior state outright.
	writeGeminiUsageLimitState(cachePath, geminiUsageLimitState{
		Severity:     geminiLimitApproaching,
		ObservedAt:   later.UTC().Format(time.RFC3339),
		ObservedAtMs: later.UnixMilli(),
	}, "fp-2")
	raw, _ = os.ReadFile(cachePath)
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("cache is not valid JSON after account switch: %v", err)
	}
	if state.Severity != geminiLimitApproaching || state.AccountFingerprint != "fp-2" {
		t.Errorf("account switch must overwrite prior state; got sev=%q fp=%q", state.Severity, state.AccountFingerprint)
	}
}

// TestLoadGeminiUsageLimitState_TTLAndAccountPinning pins the read path: a
// state older than geminiLimitNoticeTTL, or pinned to a different account,
// must not be returned.
func TestLoadGeminiUsageLimitState_TTLAndAccountPinning(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "gemini_usage_limit.json")
	t.Setenv("AIEXPEDITE_GEMINI_LIMIT_CACHE", cachePath)

	base := time.Unix(1_700_000_000, 0)
	writeGeminiUsageLimitState(cachePath, geminiUsageLimitState{
		Severity:     geminiLimitReached,
		ObservedAt:   base.UTC().Format(time.RFC3339),
		ObservedAtMs: base.UnixMilli(),
	}, "fp-1")

	if _, ok := loadGeminiUsageLimitState("fp-1", base.Add(time.Hour)); !ok {
		t.Error("live state for the same account must load")
	}
	if _, ok := loadGeminiUsageLimitState("fp-other", base.Add(time.Hour)); ok {
		t.Error("state pinned to another account must not load")
	}
	if _, ok := loadGeminiUsageLimitState("fp-1", base.Add(geminiLimitNoticeTTL+time.Minute)); ok {
		t.Error("state older than the notice TTL must not load")
	}
}
