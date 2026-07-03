package main

import (
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestDetectClaudeAuthFailure(t *testing.T) {
	cases := []struct {
		name         string
		line         string
		wantCategory string // "" means expect no detection
	}{
		{
			name:         "api_retry authentication_failed",
			line:         `{"type":"system","subtype":"api_retry","error":"authentication_failed","attempt":1,"max_retries":5}`,
			wantCategory: "authentication_failed",
		},
		{
			name:         "api_retry oauth_org_not_allowed",
			line:         `{"type":"system","subtype":"api_retry","error":"oauth_org_not_allowed"}`,
			wantCategory: "oauth_org_not_allowed",
		},
		{
			name:         "api_retry rate_limit is NOT an auth failure",
			line:         `{"type":"system","subtype":"api_retry","error":"rate_limit"}`,
			wantCategory: "",
		},
		{
			name:         "api_retry overloaded is NOT an auth failure",
			line:         `{"type":"system","subtype":"api_retry","error":"overloaded"}`,
			wantCategory: "",
		},
		{
			name:         "api_retry billing_error is NOT an auth failure (handled elsewhere)",
			line:         `{"type":"system","subtype":"api_retry","error":"billing_error"}`,
			wantCategory: "",
		},
		{
			name:         "system init is not an auth failure",
			line:         `{"type":"system","subtype":"init","session_id":"abc","tools":[]}`,
			wantCategory: "",
		},
		{
			name:         "result is_error with Invalid API key / login message",
			line:         `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key · Please run /login"}`,
			wantCategory: "invalid_api_key",
		},
		{
			name:         "result is_error with 'Not logged in' in error field",
			line:         `{"type":"result","is_error":true,"error":"Not logged in. Please run /login"}`,
			wantCategory: "invalid_api_key",
		},
		{
			name:         "result is_error but non-auth message is ignored",
			line:         `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Tool execution failed: file not found"}`,
			wantCategory: "",
		},
		{
			name:         "successful result is not an auth failure",
			line:         `{"type":"result","is_error":false,"result":"done"}`,
			wantCategory: "",
		},
		{
			name:         "assistant text delta is not an auth failure",
			line:         `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`,
			wantCategory: "",
		},
		{
			name:         "rate_limit_event is not an auth failure",
			line:         `{"type":"rate_limit_event","rate_limit":{"status":"allowed"}}`,
			wantCategory: "",
		},
		{
			name:         "non-JSON passthrough line",
			line:         "some plain stderr text",
			wantCategory: "",
		},
		{
			name:         "empty line",
			line:         "",
			wantCategory: "",
		},
		{
			name:         "malformed JSON",
			line:         `{"type":"system","subtype":`,
			wantCategory: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectClaudeAuthFailure(tc.line)
			if tc.wantCategory == "" {
				if got != nil {
					t.Fatalf("expected no auth failure, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected auth failure category %q, got nil", tc.wantCategory)
			}
			if got.Category != tc.wantCategory {
				t.Fatalf("expected category %q, got %q", tc.wantCategory, got.Category)
			}
		})
	}
}

// watchClaudeFirstFrame tests without spawning a real claude: Process is nil
// (the watchdog's Kill is nil-guarded) and only the channels/status the
// watchdog touches are populated. Mirrors the grok ACP watchdog fixture.
func newClaudeWatchdogFixtureSession(id string) *CLISession {
	return &CLISession{
		ID:             id,
		Command:        "claude",
		WorkspaceID:    "ws",
		UID:            "uid",
		Status:         "running",
		done:           make(chan struct{}),
		streamDone:     make(chan struct{}),
		firstRealFrame: make(chan struct{}),
	}
}

// TestWatchClaudeFirstFrame_FiresOnSilence pins the fail-fast: a claude session
// that never emits a real frame within the budget produces exactly one error
// frame pointing the user at `/login`, instead of hanging until the 6h stale GC.
func TestWatchClaudeFirstFrame_FiresOnSilence(t *testing.T) {
	sm := &SessionManager{}
	session := newClaudeWatchdogFixtureSession("silent-1")

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	sm.watchClaudeFirstFrame(session, publishFn, 30*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected exactly 1 published frame, got %d: %+v", len(captured), captured)
	}
	got := captured[0]
	if got.Status != "error" {
		t.Fatalf("expected status error, got %q", got.Status)
	}
	if got.SessionID != "silent-1" {
		t.Fatalf("expected SessionID silent-1, got %q", got.SessionID)
	}
	if !strings.Contains(got.Output, "no output") || !strings.Contains(got.Output, "/login") {
		t.Fatalf("expected actionable not-signed-in message, got %q", got.Output)
	}
}

// TestWatchClaudeFirstFrame_SilentWhenFrameArrives ensures a healthy session
// (claude emits real output before the budget) disarms the watchdog with no
// spurious error frame.
func TestWatchClaudeFirstFrame_SilentWhenFrameArrives(t *testing.T) {
	sm := &SessionManager{}
	session := newClaudeWatchdogFixtureSession("healthy-1")

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		session.signalFirstRealFrame()
	}()
	sm.watchClaudeFirstFrame(session, publishFn, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 0 {
		t.Fatalf("expected no published frames for a healthy session, got %d: %+v", len(captured), captured)
	}
}

// TestWatchClaudeFirstFrame_SilentWhenSessionExits ensures a session that exits
// on its own before any frame (e.g. immediate crash) does NOT get a watchdog
// error — waitForExit owns the terminal session_ended frame in that case.
func TestWatchClaudeFirstFrame_SilentWhenSessionExits(t *testing.T) {
	sm := &SessionManager{}
	session := newClaudeWatchdogFixtureSession("exited-1")

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		close(session.done)
	}()
	sm.watchClaudeFirstFrame(session, publishFn, 5*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 0 {
		t.Fatalf("expected no published frames when the session exits first, got %d: %+v", len(captured), captured)
	}
}

// TestSignalFirstRealFrame_Idempotent ensures multiple content frames don't
// panic on a double channel close.
func TestSignalFirstRealFrame_Idempotent(t *testing.T) {
	session := newClaudeWatchdogFixtureSession("idem-1")
	session.signalFirstRealFrame()
	session.signalFirstRealFrame() // must not panic (sync.Once)

	select {
	case <-session.firstRealFrame:
	default:
		t.Fatalf("expected firstRealFrame to be closed after signal")
	}
}

// TestSignalFirstRealFrame_NilSafe ensures a session without the channel (never
// happens in prod, but guards the nil check) does not panic.
func TestSignalFirstRealFrame_NilSafe(t *testing.T) {
	session := &CLISession{ID: "nil-1"}
	session.signalFirstRealFrame() // must not panic
}

// TestArmClaudeFirstFrameWatchdog_Idempotent guards the fix that lifted the
// watchdog off of process-start and onto the first prompt delivery. If either
// StartSession's initial-prompt path or SendInput's first-send path arms it,
// subsequent arms must be no-ops — otherwise a session that both sent an
// initial prompt AND received a follow-up SendInput would spawn two watchdogs
// and publish two error frames.
func TestArmClaudeFirstFrameWatchdog_Idempotent(t *testing.T) {
	sm := &SessionManager{}
	session := newClaudeWatchdogFixtureSession("arm-idem")

	var mu sync.Mutex
	var captured []resultMsg
	session.publishFn = func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	sm.armClaudeFirstFrameWatchdog(session, 20*time.Millisecond)
	sm.armClaudeFirstFrameWatchdog(session, 20*time.Millisecond)
	sm.armClaudeFirstFrameWatchdog(session, 20*time.Millisecond)

	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected exactly 1 watchdog error frame across repeated arms, got %d", len(captured))
	}
}

// TestArmClaudeFirstFrameWatchdog_NotArmedWithoutPrompt pins the regression the
// P1 review flagged: a claude session opened without an initial prompt (chat-
// direct/session.sendInput flow) must NOT be killed by the watchdog until a
// prompt has been delivered — otherwise the healthy idle case dies at 120s.
// We assert by driving a fixture session that no code calls arm on, waiting
// past the timeout, and confirming zero frames were published.
func TestArmClaudeFirstFrameWatchdog_NotArmedWithoutPrompt(t *testing.T) {
	session := newClaudeWatchdogFixtureSession("noprompt-1")

	var mu sync.Mutex
	var captured []resultMsg
	session.publishFn = func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	// Deliberately do NOT call armClaudeFirstFrameWatchdog — mirrors the
	// StartSession path when stdinPrompt == "" and SendInput has not yet
	// fired. A short wait past a would-be timeout proves no watchdog is
	// running.
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 0 {
		t.Fatalf("expected zero frames for an unarmed session, got %d: %+v", len(captured), captured)
	}
}

// TestWatchClaudeFirstFrame_PublishesBeforeKill pins the ordering fix from
// Codex review 4621910220: the watchdog error frame must reach publishFn
// BEFORE the child is killed, so a slow publish cannot race with the
// session_ended frame that waitForExit publishes once Kill unblocks
// Process.Wait(). The invariant (nothing published after session_ended) is
// enforced by session_integration_test.go's assertLifecycleOrdering.
//
// We prove ordering by spawning a real long-sleep child, then having
// publishFn probe `session.Process.Process.Signal(syscall.Signal(0))` — on
// unix that returns nil while the process is alive and errors once the
// kernel has reaped a killed child. If publish precedes Kill, the probe sees
// a live process. Skipped on Windows: syscall.Signal(0) has no equivalent
// there, and this is a race the current callsite exhibits the same on both.
func TestWatchClaudeFirstFrame_PublishesBeforeKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal(0) liveness probe is unix-only; ordering fix is identical on windows")
	}

	sm := &SessionManager{}
	session := newClaudeWatchdogFixtureSession("order-1")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start sleep child: %v", err)
	}
	session.Process = cmd
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	var mu sync.Mutex
	var aliveAtPublish bool
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		// Signal 0 asks the kernel whether the target still exists; nil means
		// the process is alive (Kill has not yet run, or has not yet been
		// scheduled). A non-nil error would mean the child was already killed
		// and reaped before publish — the bug the fix guards against.
		aliveAtPublish = cmd.Process.Signal(syscall.Signal(0)) == nil
	}

	sm.watchClaudeFirstFrame(session, publishFn, 30*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !aliveAtPublish {
		t.Fatalf("expected child to still be alive when publishFn is called (publish must precede Kill)")
	}
}

// TestArmClaudeFirstFrameWatchdog_NoopForNonClaude ensures the arm helper is
// safely a no-op for codex/gemini/grok — only claude has the not-signed-in
// stall signature this watchdog targets.
func TestArmClaudeFirstFrameWatchdog_NoopForNonClaude(t *testing.T) {
	sm := &SessionManager{}
	session := newClaudeWatchdogFixtureSession("codex-1")
	session.Command = "codex"

	var mu sync.Mutex
	var captured []resultMsg
	session.publishFn = func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	sm.armClaudeFirstFrameWatchdog(session, 20*time.Millisecond)
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 0 {
		t.Fatalf("expected zero frames for non-claude session, got %d", len(captured))
	}
}
