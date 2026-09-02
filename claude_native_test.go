package main

// Tests for ClaudeNativeManager. The integration tests drive the manager
// against the test binary re-exec'd in TEST_MOCK_CLI_MODE=claude-native-echo
// (see runMockClaudeNative below + the dispatch in session_integration_test.go)
// so we don't need a real `claude` install.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runMockClaudeNative mimics `claude --output-format stream-json
// --input-format stream-json`: it reads NDJSON user-message envelopes on stdin
// and, for each one, streams a couple of text_delta frames plus a final result
// frame that echoes the user's content. It writes a startup diagnostic to
// stderr (forwarded as claude_native_stderr) and exits when stdin closes.
func runMockClaudeNative() {
	fmt.Fprintln(os.Stderr, "mock-claude: stream-json session ready")
	reader := bufio.NewReader(os.Stdin)
	for {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var env struct {
				Type    string `json:"type"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			_ = json.Unmarshal([]byte(trimmed), &env)
			content := env.Message.Content
			fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Echo: "}}}`)
			fmt.Printf(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":%s}}}`+"\n",
				jsonEscapeString(content))
			fmt.Printf(`{"type":"result","subtype":"success","result":%s}`+"\n",
				jsonEscapeString("Echo: "+content))
		}
		if err != nil {
			break // stdin closed
		}
	}
	os.Exit(0)
}

/* -------------------------------------------------------------------------- */
/* Unit tests (no child process)                                              */
/* -------------------------------------------------------------------------- */

func TestClaudeUserEnvelope_Shape(t *testing.T) {
	got := claudeUserEnvelope("sess-1", `hi "there"`)
	var probe struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(got), &probe); err != nil {
		t.Fatalf("envelope is not valid JSON: %v (%s)", err, got)
	}
	if probe.Type != "user" || probe.Message.Role != "user" {
		t.Errorf("unexpected envelope roles: %+v", probe)
	}
	if probe.Message.Content != `hi "there"` {
		t.Errorf("content not round-tripped: %q", probe.Message.Content)
	}
	if probe.SessionID != "sess-1" {
		t.Errorf("session_id mismatch: %q", probe.SessionID)
	}
}

func TestIsClaudeNativeCommand(t *testing.T) {
	for _, ok := range []string{"claude_native_start", "claude_native_send", "claude_native_end"} {
		if !isClaudeNativeCommand(ok) {
			t.Errorf("expected %q to be a claude native command", ok)
		}
	}
	for _, no := range []string{"session_start", "codex_appserver_start", "grok_acp_start", "execute", ""} {
		if isClaudeNativeCommand(no) {
			t.Errorf("expected %q NOT to be a claude native command", no)
		}
	}
}

func TestClaudeNativeManager_Send_NotFound(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	err := m.Send("missing", "hello")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected `not found` error; got %v", err)
	}
}

func TestClaudeNativeManager_Send_Empty(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	id := "empty-fixture"
	m.sessions[id] = &ClaudeNativeSession{ID: id, status: "running", done: make(chan struct{}), streamDone: make(chan struct{})}
	if err := m.Send(id, "   "); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected `empty` error; got %v", err)
	}
}

func TestClaudeNativeManager_Send_EndedSession(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	id := "ended-fixture"
	fixture := &ClaudeNativeSession{ID: id, status: "ended", done: make(chan struct{}), streamDone: make(chan struct{})}
	close(fixture.done)
	close(fixture.streamDone)
	m.sessions[id] = fixture
	if err := m.Send(id, "hello"); err == nil || !strings.Contains(err.Error(), "has ended") {
		t.Fatalf("expected `has ended` error; got %v", err)
	}
}

func TestClaudeNativeManager_StartRejectsDuplicateID(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	id := "dupe-fixture"
	m.sessions[id] = &ClaudeNativeSession{ID: id, status: "running", done: make(chan struct{}), streamDone: make(chan struct{})}
	err := m.Start(id, t.TempDir(), nil, "", "ws", "uid", func(resultMsg) {}, nil)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected `already exists` error; got %v", err)
	}
}

func TestClaudeNativeManager_StartRequiresIDAndPublish(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	cwd := t.TempDir()
	if err := m.Start("", cwd, nil, "", "ws", "uid", func(resultMsg) {}, nil); err == nil {
		t.Fatalf("expected error for empty sessionID")
	}
	if err := m.Start("x", cwd, nil, "", "ws", "uid", nil, nil); err == nil {
		t.Fatalf("expected error for nil publishFn")
	}
}

func TestClaudeNativeManager_StartRequiresValidCwd(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	publishFn := func(resultMsg) {}

	t.Run("empty_cwd_rejected", func(t *testing.T) {
		if err := m.Start("a", "", nil, "", "ws", "uid", publishFn, nil); err == nil || !strings.Contains(err.Error(), "cwd is required") {
			t.Fatalf("expected `cwd is required`; got %v", err)
		}
	})
	t.Run("relative_cwd_rejected", func(t *testing.T) {
		if err := m.Start("b", "./rel", nil, "", "ws", "uid", publishFn, nil); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("expected `absolute path`; got %v", err)
		}
	})
	t.Run("missing_dir_rejected", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "definitely-missing-xyz123")
		if err := m.Start("c", missing, nil, "", "ws", "uid", publishFn, nil); err == nil || !strings.Contains(err.Error(), "not accessible") {
			t.Fatalf("expected `not accessible`; got %v", err)
		}
	})
	if m.ActiveCount() != 0 {
		t.Errorf("no session should register after rejected Start; got %d", m.ActiveCount())
	}
}

func TestClaudeNativeManager_EndStaleSessions_RetainsWatcherOwnedSession(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	old := &ClaudeNativeSession{ID: "old", status: "running", StartedAt: time.Now().Add(-2 * time.Hour), processExited: make(chan struct{}), done: make(chan struct{}), streamDone: make(chan struct{})}
	fresh := &ClaudeNativeSession{ID: "fresh", status: "running", StartedAt: time.Now(), processExited: make(chan struct{}), done: make(chan struct{}), streamDone: make(chan struct{})}
	// Model an exited process whose watcher is still draining streams/artifacts.
	// Stale GC may request End, but only that watcher may eventually publish the
	// terminal frame and release the ID.
	close(old.processExited)
	close(fresh.processExited)
	m.sessions["old"] = old
	m.sessions["fresh"] = fresh

	m.endStaleSessions(1 * time.Hour)

	if got := m.sessions["old"]; got != old {
		t.Errorf("stale session must remain reserved for its watcher")
	}
	if _, ok := m.sessions["fresh"]; !ok {
		t.Errorf("fresh session should survive")
	}
}

/* -------------------------------------------------------------------------- */
/* Integration tests (mock child process)                                     */
/* -------------------------------------------------------------------------- */

func skipIfUnsupportedOS(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
}

// newMockClaudeDir returns a throwaway directory to install the mock `claude`
// binary into. It deliberately does NOT use t.TempDir(): that cleanup FAILS the
// test when RemoveAll cannot delete the directory, and on Windows deleting a
// running image is illegal (unix happily unlinks one). The background
// credential probe spawns the resolved `claude` path (`auth status --json`), so
// a probe still in flight when the test's assertions finish holds the mock
// .exe open for a moment longer — which failed
// TestManagedClaudeSession_StderrAfterResultDoesNotReopenTheTurn on
// windows-latest with "Access is denied" on a test that had otherwise passed.
// Retry briefly, then leave the directory to the suite-wide sandbox teardown
// (TMPDIR/TEMP/TMP point inside it, see sandboxTestConfigDir) rather than
// failing a green test over a transient lock.
func newMockClaudeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "aix-mock-claude-")
	if err != nil {
		t.Fatalf("create mock claude dir: %v", err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		var rmErr error
		for {
			if rmErr = os.RemoveAll(dir); rmErr == nil {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("mock claude dir %s still locked (%v); left for the sandbox teardown", dir, rmErr)
	})
	return dir
}

func installMockClaude(t *testing.T, mode string) string {
	t.Helper()
	testExe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tmpDir := newMockClaudeDir(t)
	mockName := "claude"
	if runtime.GOOS == "windows" {
		mockName += ".exe"
	}
	if err := copyTestBinary(testExe, filepath.Join(tmpDir, mockName)); err != nil {
		t.Fatalf("copy mock binary: %v", err)
	}
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(mockCLIEnvVar, mode)
	// resolveExecutable("claude") memoizes via cachedResolveClaudePath, so
	// reset the once/cache to this test's PATH and restore it afterwards —
	// otherwise a second integration test reuses the first test's (now-deleted)
	// tempdir path and fork/exec fails with ENOENT.
	resetCachedClaudePath()
	t.Cleanup(resetCachedClaudePath)
	return tmpDir
}

func TestClaudeNativeLifecycle_StartSendEnd(t *testing.T) {
	skipIfUnsupportedOS(t)
	tmpDir := installMockClaude(t, "claude-native-echo")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-test-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	// Start with an initial prompt delivered on stdin.
	if err := m.Start(id, tmpDir, nil, "hello world", "ws", "uid", publishFn, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	messagesContain := func(want string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, msg := range captured {
			if msg.Type == "claude_native_message" && strings.Contains(msg.Output, want) {
				return true
			}
		}
		return false
	}
	waitFor := func(what string, pred func() bool) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if pred() {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	// The first turn's stream-json frames (verbatim) must reach the publish
	// stream, including the echoed prompt content.
	waitFor("first turn echo", func() bool { return messagesContain("hello world") })

	// stderr forwarding.
	waitFor("stderr forwarded", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, msg := range captured {
			if msg.Type == "claude_native_stderr" && strings.Contains(msg.Output, "mock-claude") {
				return true
			}
		}
		return false
	})

	// Second turn via Send.
	if err := m.Send(id, "second turn please"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitFor("second turn echo", func() bool { return messagesContain("second turn please") })

	// End → claude_native_ended must be the terminal frame and the session
	// must be reclaimed.
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}
	waitFor("ended frame", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(captured) > 0 && captured[len(captured)-1].Type == "claude_native_ended"
	})

	mu.Lock()
	last := captured[len(captured)-1]
	mu.Unlock()
	if last.Type != "claude_native_ended" || last.SessionID != id {
		t.Errorf("expected final claude_native_ended for %q; got type=%q session=%q", id, last.Type, last.SessionID)
	}
	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active sessions after End; got %d", m.ActiveCount())
	}
}

func TestClaudeNativeLifecycle_OversizeFrameTerminatesSession(t *testing.T) {
	skipIfUnsupportedOS(t)
	tmpDir := installMockClaude(t, "claude-native-oversize")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-oversize-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "go", "ws", "uid", publishFn, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	sawFatal := false
	for time.Now().Before(deadline) && !sawFatal {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type == "claude_native_error" && strings.Contains(msg.Output, "publishable limit") {
				sawFatal = true
				break
			}
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
	}
	if !sawFatal {
		t.Fatalf("expected a fatal claude_native_error for the oversize frame")
	}
	_ = m.End(id)
}

// A hard-quota rejection surfaced by captureClaudeRateLimitLine must be
// republished as a dedicated `claude_native_ratelimit` frame whose Output
// carries formatClaudeLimitLine. The legacy Claude session path emits this so
// agent-orchestrator-service's detectRateLimit can defer + auto-resume at the
// reset instant; without it native sessions that hit Claude limits finish/fail
// instead of being rescheduled.
func TestClaudeNativeLifecycle_SurfacesRejectedRateLimit(t *testing.T) {
	skipIfUnsupportedOS(t)
	// Isolate the rate-limit cache so the mock's rejected event doesn't pollute
	// this user's real snapshot.
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tmpDir := installMockClaude(t, "claude-native-ratelimit")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-ratelimit-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	publishFn := func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}

	if err := m.Start(id, tmpDir, nil, "trigger", "ws", "uid", publishFn, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })

	deadline := time.Now().Add(15 * time.Second)
	sawLimit := false
	for time.Now().Before(deadline) && !sawLimit {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type == "claude_native_ratelimit" &&
				strings.Contains(msg.Output, "usage limit") &&
				strings.Contains(msg.Output, "resets at") {
				sawLimit = true
				break
			}
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
	}
	if !sawLimit {
		t.Fatalf("expected a claude_native_ratelimit frame carrying formatClaudeLimitLine text")
	}
}

func TestClaudeNativeLifecycle_StartFailsWhenBinaryMissing(t *testing.T) {
	skipIfUnsupportedOS(t)
	// Point PATH at an empty dir so resolveExecutable("claude") can't find it.
	tmpDir := t.TempDir()
	isolateTestUserHome(t, tmpDir)
	t.Setenv("PATH", tmpDir)
	resetCachedClaudePath()
	t.Cleanup(resetCachedClaudePath)

	m := NewClaudeNativeManager(nil)
	err := m.Start("nobin", t.TempDir(), nil, "hi", "ws", "uid", func(resultMsg) {}, nil)
	if err == nil {
		t.Fatalf("expected Start to fail when `claude` is not on PATH")
	}
}

// seedStaleClaudeObservation writes one real-but-old five-hour reading into the
// probe-isolated cache and returns its observation time. This is the "card is
// frozen" starting state the feature exists to unstick.
func seedStaleClaudeObservation(t *testing.T, cache string) time.Time {
	t.Helper()
	seeded := time.Now().Add(-4 * time.Hour)
	mergeClaudeRateLimitCache(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 21, ResetsAtMs: time.Now().Add(3 * time.Hour).UnixMilli(),
			ObservedAtMs: seeded.UnixMilli(), Status: "allowed", usageKnown: true,
		},
	}, seeded, "")
	return seeded
}

// waitForClaudeObservationAfter polls the cache until the five-hour window
// carries a PROBE reading observed after `after`. The probe is deliberately
// asynchronous (it must never delay frame ordering or waitForExit), so polling
// is the only honest way to assert it ran.
//
// Both conditions matter. A heartbeat that names a reset the cache has not seen
// before also writes a bucket with a current ObservedAtMs — but with
// usageObserved=false, so it reports as "unobservable, resets <date>" and moves
// no percentage. Only a real reading unsticks the card, which is exactly the
// distinction the reported bug turned on.
func waitForClaudeObservationAfter(t *testing.T, cache string, after time.Time) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := loadClaudeRateLimitSnapshot(cache); ok {
			b := snap.Buckets[claudeWindowFiveHour]
			if b.Source == claudeRateLimitSourceProbe && b.hasObservedUsage() && b.ObservedAtMs > after.UnixMilli() {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	snap, _ := loadClaudeRateLimitSnapshot(cache)
	t.Fatalf("no probe reading landed after %s; cache=%+v", after.UTC().Format(time.RFC3339Nano), snap.Buckets)
}

// Direct (chat-native) run: a heartbeat-only session that ends with a terminal
// `result` frame must leave the cache with a NEWER observation than it started
// with. Without the probe this is exactly the reported bug — the run succeeds,
// the smokes pass, and latestObservedAt never moves.
func TestClaudeNativeLifecycle_TerminalResultAdvancesUtilization(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"utilization":37,"resets_at":%d,"status":"allowed"}}`,
			time.Now().Add(3*time.Hour).Unix())
	})
	seeded := seedStaleClaudeObservation(t, cache)
	tmpDir := installMockClaude(t, "claude-heartbeat-result")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-freshness-%d", time.Now().UnixNano())
	if err := m.Start(id, tmpDir, nil, "hello", "ws", "uid", func(resultMsg) {}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })

	waitForClaudeObservationAfter(t, cache, seeded)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("probe request count=%d, want exactly 1 for a single run", got)
	}

	metric := claudeCodeMetricsFromCache(time.Now(), "")[0]
	if metric.Consumed == nil || *metric.Consumed < 36.9 || *metric.Consumed > 37.1 {
		t.Errorf("five-hour Consumed=%v, want ~37 from the probe reading", metric.Consumed)
	}
}

// A direct run that never reaches a terminal `result` frame — killed, timed out,
// a stream failure, or an exit mid-turn — still consumed quota, so the exit path
// must make its own attempt. Mirrors
// TestManagedClaudeSession_KilledRunStillProbesOnSessionEnd: both execution
// paths have to cover the abnormal exit, not just the managed one.
func TestClaudeNativeLifecycle_KilledRunStillProbesOnExit(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, _ := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"utilization":72,"resets_at":%d,"status":"allowed"}}`,
			time.Now().Add(3*time.Hour).Unix())
	})
	seeded := seedStaleClaudeObservation(t, cache)
	tmpDir := installMockClaude(t, "claude-heartbeat-hang")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-killed-%d", time.Now().UnixNano())
	if err := m.Start(id, tmpDir, nil, "hello", "ws", "uid", func(resultMsg) {}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the usage-less heartbeat land, then kill the run mid-turn.
	time.Sleep(200 * time.Millisecond)
	if err := m.End(id); err != nil {
		t.Fatalf("End: %v", err)
	}

	waitForClaudeObservationAfter(t, cache, seeded)
}

// Direct run that ends on its terminal `result` frame: exactly one request, and
// no post-run debt left behind. See waitForNoOutstandingClaudeUsageDebt for what
// an unconditional exit trigger costs — single-flight collapses overlapping
// requests, never the debt, so a second OAuth call gets scheduled a minute later
// for a run nothing else happened on.
func TestClaudeNativeLifecycle_SettledTurnLeavesNoTrailingDebt(t *testing.T) {
	skipIfUnsupportedOS(t)
	cache, calls := armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"utilization":37,"resets_at":%d,"status":"allowed"}}`,
			time.Now().Add(3*time.Hour).Unix())
	})
	seeded := seedStaleClaudeObservation(t, cache)
	// A production-shaped interval, so a debt left outstanding stays outstanding
	// rather than being paid instantly by a zero-interval retry.
	t.Setenv(claudeUsageProbeMinIntervalEnv, "60000")
	tmpDir := installMockClaude(t, "claude-heartbeat-result-linger")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-settled-%d", time.Now().UnixNano())
	if err := m.Start(id, tmpDir, nil, "hello", "ws", "uid", func(resultMsg) {}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })

	waitForClaudeObservationAfter(t, cache, seeded)
	waitForNoOutstandingClaudeUsageDebt(t, 5*time.Second, 1500*time.Millisecond)
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("probe request count=%d, want exactly 1 for a single settled turn", got)
	}
}

// The rate-limit rejection path must be untouched by the probe wiring: a
// rejected run still publishes the `claude_native_ratelimit` frame that
// agent-orchestrator-service's detectRateLimit defers and auto-resumes on.
func TestClaudeNativeLifecycle_RejectionFrameSurvivesProbeWiring(t *testing.T) {
	skipIfUnsupportedOS(t)
	_, _ = armClaudeUsageProbe(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"five_hour":{"utilization":1.0,"resets_at":%d,"status":"rejected"}}`,
			time.Now().Add(45*time.Minute).Unix())
	})
	tmpDir := installMockClaude(t, "claude-native-ratelimit")

	m := NewClaudeNativeManager(nil)
	id := fmt.Sprintf("claude-reject-probe-%d", time.Now().UnixNano())

	var mu sync.Mutex
	var captured []resultMsg
	if err := m.Start(id, tmpDir, nil, "trigger", "ws", "uid", func(res resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		captured = append(captured, res)
	}, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, msg := range captured {
			if msg.Type == "claude_native_ratelimit" &&
				strings.Contains(msg.Output, "usage limit") &&
				strings.Contains(msg.Output, "resets at") {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("expected the claude_native_ratelimit frame to still publish unchanged")
}

// A follow-up turn on a live session must reopen the utilization debt. The
// stdout scanner is the only other writer of turnSettled and it clears the flag
// per line — so a session that emitted `result`, accepted a follow-up, and was
// then killed before producing its next frame still looked "settled" to
// waitForExit, which skipped the abnormal-exit probe and dropped that turn's
// quota from the card.
func TestClaudeNativeWriteUserTurnClearsSettledTurn(t *testing.T) {
	m := NewClaudeNativeManager(nil)
	session := &ClaudeNativeSession{
		ID:            "followup-turn",
		status:        "running",
		Stdin:         nopWriteCloser{},
		processExited: make(chan struct{}),
		done:          make(chan struct{}),
		streamDone:    make(chan struct{}),
	}
	// The prior turn ended on its `result` frame, exactly as readStream leaves it.
	session.turnSettled.Store(true)

	if err := m.writeUserTurn(session, "one more thing"); err != nil {
		t.Fatalf("writeUserTurn: %v", err)
	}
	if session.turnSettled.Load() {
		t.Fatal("turnSettled must be cleared when a follow-up turn is accepted, " +
			"otherwise an abnormal exit before the next frame skips the utilization probe")
	}
}
