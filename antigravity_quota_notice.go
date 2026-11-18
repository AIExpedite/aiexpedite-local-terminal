// File: antigravity_quota_notice.go
// -----------------------------------------------------------------------------
// Surfaces an Antigravity (`agy`) quota exhaustion as a limit notice the cloud
// can act on, and stops the CLI's silent retry loop.
//
// WHAT AGY DOES WHEN THE ACCOUNT IS OUT OF QUOTA (observed 2026-09-18, agy
// 1.2.6, Windows): the stream-json output carries only
//
//	{"event":"step_update","step_update":{"step_type":"error_message", ...}}
//
// steps with NO text, one per attempt, while the CLI retries with a growing
// backoff (4s, 7s, 10s, 21s, 58s, …). The reason exists in exactly one place,
// its own log at ~/.gemini/antigravity-cli/cli.log:
//
//	I0918 10:02:56.745522   460 run.go:395] Run: attempt 1 failed
//	  (RESOURCE_EXHAUSTED (code 429): Individual quota reached. Please upgrade
//	  your subscription to increase your limits. Resets in 1h27m57s.),
//	  retrying in 4s
//
// So the platform saw a session that produced nothing for as long as its turn
// budget allowed — hours, on an execution — instead of the limit notice that
// makes it park or re-route (shared-constants `CLI_AGENT_LIMIT_SIGNALS`,
// ai-service `pauseIfAuthorRateLimited`).
//
// THE FIX. When a managed agy turn reports an `error_message` step, read the
// tail of cli.log for a `Run: … failed (<reason>)` line written since this
// process started. A QUOTA reason (RESOURCE_EXHAUSTED / 429) is published in
// the wrapper the contract already expects — `[Antigravity turn failed:
// <reason>]`, the same frame a result.status ERROR takes — the turn is marked
// failed, and the process is interrupted so the retry loop does not hold the
// device. A native turn (antigravity_native.go) follows its OWN log block once a
// second instead (antigravity_turn_monitor.go) and interrupts the same way;
// after it exits, its block's failure lines feed the turn's failure classifier
// (antigravity_turn_failure.go) whatever their reason.
//
// For a RAW session the log is shared by every agy process on the machine and
// names no conversation, so a line is matched by TIME (since this spawn) and
// only acted on when THIS process is failing. The quota is per account, so a neighbour's
// 429 in that window means the same thing for this turn. A reset under
// TWO minutes is a throttle agy rides out itself and is left alone.
// -----------------------------------------------------------------------------

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Bytes read from the end of cli.log per lookup — a few hundred lines.
	antigravityCliLogTailBytes = 256 * 1024
	// A "Resets in …" shorter than this is a throttle, not a limit.
	antigravityQuotaThrottleWindow = 2 * time.Minute
	// Grace between the interrupt and the kill that stops the retry loop.
	antigravityQuotaKillGrace = 3 * time.Second
	// Tolerated drift between the log clock and ours — both when deciding a
	// line predates this turn and when dating a year-less stamp.
	antigravityLogClockSkew = 2 * time.Second
)

// glog line: level, mmdd, hh:mm:ss.micros, thread, file:line] message.
var antigravityGlogRecord = regexp.MustCompile(
	`^[IWEF](\d{2})(\d{2}) (\d{2}):(\d{2}):(\d{2})\.(\d{1,6})\s+\d+\s+\S+\] (.*)$`,
)

// antigravityRunFailureMessages are the messages a run's failure line takes,
// each capturing the reason:
//
//   - agy ≤ 1.2.x retries and logs every attempt:
//     `Run: attempt 1 failed (<reason>), retrying in 4s`;
//   - agy 1.5.10 (AIE2, 2026-11-17) does not retry; it logs
//     `agent executor error: generating and executing: <reason>` and then
//     `Print mode: run ended with error and no response: <reason>`, and
//     exits 3.
var antigravityRunFailureMessages = []*regexp.Regexp{
	regexp.MustCompile(`^Run: (?:attempt \d+ )?failed \((.+)\)(?:, retrying in .*)?\s*$`),
	regexp.MustCompile(`^Print mode: run ended with error and no response: (.+?)\s*$`),
	regexp.MustCompile(`^agent executor error: generating and executing: (.+?)\s*$`),
}

// antigravityExecutorErrorPrefix is folded off a Print-mode reason that
// repeats the executor error, so both lines yield the same reason.
const antigravityExecutorErrorPrefix = "agent executor error: generating and executing: "

// `Resets in 1h27m57s` / `Resets in 27m` / `resets in 1h 27m 57s`.
var antigravityResetsIn = regexp.MustCompile(`(?i)\bresets? in ((?:\d+\s*[dhms]\s*)+)`)

// Test seams: the log path and the clock the lookups compare against.
var (
	antigravityCliLogPathOverride string
	antigravityQuotaClock         = time.Now
)

func antigravityCliLogPath() string {
	if antigravityCliLogPathOverride != "" {
		return antigravityCliLogPathOverride
	}
	base := antigravityHomeBase()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "cli.log")
}

// isAntigravityErrorMessageStep reports whether a stdout line is one of the
// text-less error steps agy emits per failed attempt.
func isAntigravityErrorMessageStep(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") || !strings.Contains(trimmed, "error_message") {
		return false
	}
	var ev antigravityStreamEvent
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		return false
	}
	return ev.Event == "step_update" && ev.StepUpdate != nil && ev.StepUpdate.StepType == "error_message"
}

// isAntigravityQuotaReason reports whether a run-failure reason is the account
// being out of quota (as opposed to a network blip or a tool failure agy also
// retries). The Google code and the HTTP status are the stable signals; the
// prose has already changed between builds.
func isAntigravityQuotaReason(reason string) bool {
	r := strings.ToLower(reason)
	return strings.Contains(r, "resource_exhausted") ||
		strings.Contains(r, "(code 429)") ||
		strings.Contains(r, "quota")
}

// antigravityResetWindow parses the `Resets in …` clock out of a reason, when
// it names one. Zero, false when it does not.
func antigravityResetWindow(reason string) (time.Duration, bool) {
	m := antigravityResetsIn.FindStringSubmatch(reason)
	if m == nil {
		return 0, false
	}
	d, err := time.ParseDuration(strings.ReplaceAll(m[1], " ", ""))
	if err != nil {
		return 0, false
	}
	return d, true
}

// parseAntigravityRunFailureLine reads one cli.log line. glog stamps no year:
// the current one is assumed, stepping back a year when that lands in the
// future (a lookup across New Year). ANY stamp ahead of now is last year’s —
// on January 1 a line dated January 2 is a year old, not tomorrow’s — bar
// antigravityLogClockSkew of drift, which keeps a line agy has only just
// written eligible.
func parseAntigravityRunFailureLine(line string, now time.Time) (at time.Time, reason string, ok bool) {
	m := antigravityGlogRecord.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil {
		return time.Time{}, "", false
	}
	for _, re := range antigravityRunFailureMessages {
		if r := re.FindStringSubmatch(m[7]); r != nil {
			reason = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r[1]), antigravityExecutorErrorPrefix))
			break
		}
	}
	if reason == "" {
		return time.Time{}, "", false
	}
	month, _ := strconv.Atoi(m[1])
	day, _ := strconv.Atoi(m[2])
	hour, _ := strconv.Atoi(m[3])
	minute, _ := strconv.Atoi(m[4])
	second, _ := strconv.Atoi(m[5])
	// Fraction digits are a decimal fraction: `.5` is 500000µs, not 5µs.
	micro, _ := strconv.Atoi(m[6] + strings.Repeat("0", 6-len(m[6])))
	at = time.Date(now.Year(), time.Month(month), day, hour, minute, second, micro*1000, now.Location())
	if at.After(now.Add(antigravityLogClockSkew)) {
		at = at.AddDate(-1, 0, 0)
	}
	return at, reason, true
}

// antigravityRunFailureReasons returns every failure reason in one run's own
// log block (antigravityPIDBlock), quota or not, oldest first and each once.
// The classifier reads them as the run's failure lines
// (antigravity_turn_failure.go); they are never a neighbour's, because the
// block is this run's alone.
func antigravityRunFailureReasons(block []byte) []string {
	var out []string
	seen := map[string]bool{}
	now := antigravityQuotaClock()
	for _, raw := range bytes.Split(block, []byte("\n")) {
		_, reason, ok := parseAntigravityRunFailureLine(string(raw), now)
		if !ok || seen[reason] {
			continue
		}
		seen[reason] = true
		out = append(out, reason)
	}
	return out
}

// findAntigravityQuotaFailureSince returns the newest quota reason cli.log
// recorded since `since`, or "" when there is none — or when the newest one
// names a reset short enough to be a throttle.
func findAntigravityQuotaFailureSince(since, now time.Time) string {
	return findAntigravityQuotaFailureInLog(antigravityCliLogPath(), since, now)
}

func findAntigravityQuotaFailureInLog(path string, since, now time.Time) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - antigravityCliLogTailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(f, antigravityCliLogTailBytes))
	if err != nil {
		return ""
	}
	// Tolerate a cut first line and any timestamp drift between the log's
	// clock and ours by a couple of seconds.
	return newestAntigravityQuotaReason(data, since.Add(-antigravityLogClockSkew), now)
}

// newestAntigravityQuotaReason is the newest quota reason among data's
// failure lines stamped at or after cutoff, "" when there is none — or when
// it names a reset short enough to be a throttle agy rides out itself.
func newestAntigravityQuotaReason(data []byte, cutoff, now time.Time) string {
	newest := ""
	for _, raw := range bytes.Split(data, []byte("\n")) {
		at, reason, ok := parseAntigravityRunFailureLine(string(raw), now)
		if !ok || at.Before(cutoff) || !isAntigravityQuotaReason(reason) {
			continue
		}
		// A record whose own `Resets in …` window has already elapsed is spent:
		// that quota came back before now, so the line says nothing about the
		// failure being looked up. Matters because `since` can be much older
		// than the turn — a raw session is opened when the chat opens and may
		// sit idle for hours before its first prompt.
		if window, ok := antigravityResetWindow(reason); ok && !at.Add(window).After(now) {
			continue
		}
		newest = reason
	}
	if newest == "" {
		return ""
	}
	if window, ok := antigravityResetWindow(newest); ok && window < antigravityQuotaThrottleWindow {
		return ""
	}
	return newest
}

// stopAntigravityRetryLoop interrupts agy and, failing that, kills it: the
// notice is already published, and every further attempt would only hold the
// device until the turn budget ran out.
func stopAntigravityRetryLoop(cmd *exec.Cmd, kill func()) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = interruptProcess(cmd)
	time.AfterFunc(antigravityQuotaKillGrace, kill)
}

// quotaFailureFromErrorStep is the RAW-session hook (session.go
// readOutputStream): on a managed agy turn's error step, the quota reason from
// cli.log, once per session. "" means keep going — the step was not an error
// step, the failure is not a quota one, or it was already handled.
func (s *CLISession) quotaFailureFromErrorStep(line string) string {
	if !s.antigravityManagedStream || !isAntigravityErrorMessageStep(line) {
		return ""
	}
	s.mu.Lock()
	handled := s.antigravityQuotaHandled
	startedAt := s.StartedAt
	s.mu.Unlock()
	if handled {
		return ""
	}
	reason := findAntigravityQuotaFailureSince(startedAt, antigravityQuotaClock())
	if reason == "" {
		return ""
	}
	s.mu.Lock()
	if s.antigravityQuotaHandled {
		s.mu.Unlock()
		return ""
	}
	s.antigravityQuotaHandled = true
	s.mu.Unlock()
	return reason
}
