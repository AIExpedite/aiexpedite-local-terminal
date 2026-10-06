// cliagent_usage_opencode_capture.go — OpenCode's device-local usage ledger.
//
// OpenCode has no quota of its own and no usage command, but every
// `opencode run --format json` turn reports what it spent: each step closes
// with a `step_finish` frame whose part carries `tokens` and `cost`. This file
// reads those frames from every OpenCode turn this agent spawns — the
// `__cli_smoke__` probe, the resident chat (opencode_native.go) and terminal
// sessions (session.go) — and folds them into opencode_usage.json, which
// cliagent_usage_opencode.go publishes as "Tokens today (agent runs)" and
// "Cost today".
//
// Shape and invariants:
//
//   - one bucket per (account fingerprint, local calendar date), capped at
//     openCodeUsageMaxBuckets, oldest observation dropped first. Values only
//     move forward; a negative or non-finite token or cost rejects the whole
//     frame, never clamped.
//   - "Tokens" is input + output + reasoning. Cache read/write are kept per
//     bucket but not published: on a long session they dwarf real usage.
//   - a step counts once: its key is sha256(sessionID NUL part id), truncated,
//     remembered in seenSteps (capped). A frame with no part id keys on its
//     message id plus end time; one with neither counts without dedup.
//   - the generation advances only when a write changes a bucket, under this
//     process's epoch (codexProcessGenerationEpoch — one random epoch per agent
//     process). A committed advance hints the backend
//     (noteCLIUsageObservationAdvanced).
//
// Parsing a frame touches memory only: the stream goroutines never wait on the
// ledger. A run's steps are committed once, when it settles
// (cliagent_usage_opencode_freshness.go).
//
// SECRETS: the ledger holds counts, hashed step keys and the account
// fingerprint. The only raw identifier is a debt's OpenCode session id — a
// local opaque id the export fallback needs, never a credential — and it stays
// in this 0600 file. Logs are fixed labels only.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	openCodeUsageProvider      = "opencode"
	openCodeUsageSchemaVersion = 1
	openCodeUsageMaxBuckets    = 16
	openCodeUsageMaxSeenSteps  = 1024
	openCodeUsageMaxDebts      = 8
	// openCodeUsageMaxRunSteps bounds one run's in-memory accumulator. Steps past
	// it fold into the last one without a dedup key: still counted, never
	// unbounded.
	openCodeUsageMaxRunSteps = 4096
)

// openCodeUsageLedger is opencode_usage.json.
type openCodeUsageLedger struct {
	SchemaVersion int                   `json:"schemaVersion"`
	Generation    cliUsageGeneration    `json:"generation"`
	Buckets       []openCodeUsageBucket `json:"buckets,omitempty"`
	Debts         []openCodeUsageDebt   `json:"debts,omitempty"`
	SeenSteps     []string              `json:"seenSteps,omitempty"`
}

// openCodeUsageBucket is one account's spend on one local calendar day.
type openCodeUsageBucket struct {
	AccountFingerprint string  `json:"accountFingerprint"`
	LocalDate          string  `json:"localDate"`
	InputTokens        int64   `json:"inputTokens"`
	OutputTokens       int64   `json:"outputTokens"`
	ReasoningTokens    int64   `json:"reasoningTokens"`
	CacheReadTokens    int64   `json:"cacheReadTokens"`
	CacheWriteTokens   int64   `json:"cacheWriteTokens"`
	CostUsd            float64 `json:"costUsd"`
	// ObservedAtMs is the newest contributing step's end time.
	ObservedAtMs int64 `json:"observedAtMs"`
}

// tokens is what "Tokens today" publishes.
func (b openCodeUsageBucket) tokens() int64 {
	return b.InputTokens + b.OutputTokens + b.ReasoningTokens
}

// openCodeUsageDebt is a run whose usage is not in a bucket yet. Armed
// (OwedAtMs 0) from the child's spawn until it settles, so a crash or update
// mid-turn still leaves something to pay from; owed when it settled with no
// usage captured but a session id to export.
type openCodeUsageDebt struct {
	RunID              string `json:"runId"`
	RunFloorMs         int64  `json:"runFloorMs"`
	SessionID          string `json:"sessionId,omitempty"`
	AccountFingerprint string `json:"accountFingerprint,omitempty"`
	// Dir is the run's working directory: OpenCode files a session under the
	// project it resolves from cwd, so the export runs there.
	Dir         string `json:"dir,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	OwedAtMs    int64  `json:"owedAtMs,omitempty"`
	SettledAtMs int64  `json:"settledAtMs,omitempty"`
}

func (d openCodeUsageDebt) owed() bool { return d.OwedAtMs > 0 }

// openCodeUsageStep is one step's spend.
type openCodeUsageStep struct {
	// Key is the hashed dedup key, "" when the frame named nothing stable.
	Key        string
	AtMs       int64
	Input      int64
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
}

// openCodeUsageLockWait bounds how long a ledger write waits on another agent
// process holding the cross-process lock (an update hand-off). A var so tests
// can pin it small.
var openCodeUsageLockWait = 2 * time.Second

var (
	// openCodeUsageMu serialises every ledger read-modify-write in this process.
	openCodeUsageMu sync.Mutex
	// openCodeGenerationRotated reports a committed write carrying this
	// process's epoch. Until then the ledger may hold a generation an earlier
	// process published, so ParseContext omits UsageGeneration and the
	// propagator holds OpenCode hints.
	openCodeGenerationRotated atomic.Bool
	// openCodeUsageLocation is the zone the local calendar day is cut in; nil
	// means time.Local. A test seam.
	openCodeUsageLocation *time.Location
)

// openCodeUsageCachePath: AIEXPEDITE_OPENCODE_USAGE_CACHE overrides it (tests
// isolate from the real machine ledger).
func openCodeUsageCachePath() string {
	if p := os.Getenv("AIEXPEDITE_OPENCODE_USAGE_CACHE"); p != "" {
		return p
	}
	return filepath.Join(GetConfigDir(), "opencode_usage.json")
}

// readOpenCodeUsageLedger reads the ledger. A missing, unreadable, oversized
// or other-schema file reads as empty.
func readOpenCodeUsageLedger() openCodeUsageLedger {
	var ledger openCodeUsageLedger
	if !readBoundedJSONFile(openCodeUsageCachePath(), &ledger) || ledger.SchemaVersion != openCodeUsageSchemaVersion {
		return openCodeUsageLedger{SchemaVersion: openCodeUsageSchemaVersion}
	}
	return ledger
}

// openCodeUsageTransaction runs mutate as one read-modify-write of the ledger.
// mutate returns (write, changed): write=false aborts without writing;
// changed=true advances the generation. It reports whether the write committed
// and the generation after it. The caller decides whether a committed change
// is worth a hint (noteOpenCodeUsageAdvanced): a startup rotation is hinted
// only when there is something to publish.
//
// The in-process mutex alone is not enough: a self-update hands off between two
// agent processes that briefly run side by side, and each would rename its own
// read-modify-write over the other's. The cross-process sibling lock the Codex
// and Claude caches use serialises them; a contended lock refuses the write
// rather than overwrite a competitor's, and a filesystem that offers no lock
// at all degrades to the in-process mutex alone.
func openCodeUsageTransaction(mutate func(ledger *openCodeUsageLedger) (write, changed bool)) (committed, changed bool, generation cliUsageGeneration) {
	openCodeUsageMu.Lock()
	path := openCodeUsageCachePath()
	lock, outcome := acquireCrossProcessCacheLockUntil(path, time.Now().Add(openCodeUsageLockWait))
	if outcome == crossProcessLockContended {
		openCodeUsageMu.Unlock()
		logOpenCodeUsageCapture("lock_contended")
		return false, false, cliUsageGeneration{}
	}
	release := func() {
		if lock != nil {
			_ = unlockFile(lock)
			_ = lock.Close()
		}
		openCodeUsageMu.Unlock()
	}
	ledger := readOpenCodeUsageLedger()
	write, changed := mutate(&ledger)
	if !write {
		release()
		return false, false, ledger.Generation
	}
	if changed {
		openCodeBumpGeneration(&ledger)
	}
	committed = writeJSONFileAtomic(path, ledger)
	generation = ledger.Generation
	release()
	if !committed {
		logOpenCodeUsageCapture("write_failed")
		return false, false, generation
	}
	if generation.Epoch == codexProcessGenerationEpoch.Load() {
		openCodeGenerationRotated.Store(true)
	}
	return true, changed, generation
}

// noteOpenCodeUsageAdvanced hints the backend about a committed bucket change.
func noteOpenCodeUsageAdvanced(committed, changed bool, generation cliUsageGeneration) {
	if committed && changed {
		noteCLIUsageObservationAdvanced(openCodeUsageProvider, generation)
	}
}

// openCodeBumpGeneration advances the ledger's generation; a ledger from any
// other epoch restarts at counter 1 under this process's.
func openCodeBumpGeneration(ledger *openCodeUsageLedger) {
	if epoch := codexProcessGenerationEpoch.Load(); ledger.Generation.Epoch != epoch {
		ledger.Generation = cliUsageGeneration{Epoch: epoch, Counter: 1}
		return
	}
	ledger.Generation.Counter++
}

// openCodeRecoveryGeneration moves a ledger an earlier process wrote onto this
// process's epoch in one write, then returns its generation when a bucket for
// today still has numbers to publish (nil otherwise). Called once at startup
// by the propagator, so a reading captured just before an update handoff —
// whose hint died with that process — is hinted again. A ledger that never
// published anything is left for its first real write to rotate.
func openCodeRecoveryGeneration(now time.Time) *cliUsageGeneration {
	today, _ := openCodeLocalDay(now)
	hasToday := false
	_, _, generation := openCodeUsageTransaction(func(ledger *openCodeUsageLedger) (bool, bool) {
		for _, b := range ledger.Buckets {
			if b.LocalDate == today && (b.tokens() > 0 || b.CostUsd > 0) {
				hasToday = true
			}
		}
		rotate := len(ledger.Buckets) > 0 && ledger.Generation.Epoch != codexProcessGenerationEpoch.Load()
		return rotate, rotate
	})
	if !hasToday || generation.Counter <= 0 || generation.Epoch != codexProcessGenerationEpoch.Load() {
		return nil
	}
	openCodeGenerationRotated.Store(true)
	return &generation
}

// openCodeLocalDay is t's local calendar date and the next local midnight — 23
// or 25 hours away on a DST day.
func openCodeLocalDay(t time.Time) (date string, resetAt time.Time) {
	loc := openCodeUsageLocation
	if loc == nil {
		loc = time.Local
	}
	local := t.In(loc)
	y, m, d := local.Date()
	return local.Format("2006-01-02"), time.Date(y, m, d+1, 0, 0, 0, 0, loc)
}

// mergeOpenCodeUsageSteps folds steps into the ledger's buckets for
// fingerprint, skipping any whose key was already counted. It reports whether a
// bucket changed.
func mergeOpenCodeUsageSteps(ledger *openCodeUsageLedger, fingerprint string, steps []openCodeUsageStep) bool {
	seen := make(map[string]bool, len(ledger.SeenSteps))
	for _, k := range ledger.SeenSteps {
		seen[k] = true
	}
	changed := false
	for _, step := range steps {
		if step.Key != "" {
			if seen[step.Key] {
				continue
			}
			seen[step.Key] = true
			ledger.SeenSteps = append(ledger.SeenSteps, step.Key)
		}
		if step.Input+step.Output+step.Reasoning+step.CacheRead+step.CacheWrite == 0 && step.Cost == 0 {
			continue
		}
		date, _ := openCodeLocalDay(time.UnixMilli(step.AtMs))
		bucket := openCodeUsageBucketFor(ledger, fingerprint, date)
		bucket.InputTokens += step.Input
		bucket.OutputTokens += step.Output
		bucket.ReasoningTokens += step.Reasoning
		bucket.CacheReadTokens += step.CacheRead
		bucket.CacheWriteTokens += step.CacheWrite
		bucket.CostUsd += step.Cost
		if step.AtMs > bucket.ObservedAtMs {
			bucket.ObservedAtMs = step.AtMs
		}
		changed = true
	}
	if n := len(ledger.SeenSteps); n > openCodeUsageMaxSeenSteps {
		ledger.SeenSteps = append([]string(nil), ledger.SeenSteps[n-openCodeUsageMaxSeenSteps:]...)
	}
	capOpenCodeUsageBuckets(ledger)
	return changed
}

// openCodeUsageBucketFor returns the bucket for (fingerprint, date), creating it.
func openCodeUsageBucketFor(ledger *openCodeUsageLedger, fingerprint, date string) *openCodeUsageBucket {
	for i := range ledger.Buckets {
		if ledger.Buckets[i].AccountFingerprint == fingerprint && ledger.Buckets[i].LocalDate == date {
			return &ledger.Buckets[i]
		}
	}
	ledger.Buckets = append(ledger.Buckets, openCodeUsageBucket{AccountFingerprint: fingerprint, LocalDate: date})
	return &ledger.Buckets[len(ledger.Buckets)-1]
}

// capOpenCodeUsageBuckets drops the oldest observations past the cap. A bucket
// created for a long-past date by a late export lands oldest, so it is the one
// dropped.
func capOpenCodeUsageBuckets(ledger *openCodeUsageLedger) {
	for len(ledger.Buckets) > openCodeUsageMaxBuckets {
		oldest := 0
		for i, b := range ledger.Buckets {
			if b.ObservedAtMs < ledger.Buckets[oldest].ObservedAtMs {
				oldest = i
			}
		}
		ledger.Buckets = append(ledger.Buckets[:oldest], ledger.Buckets[oldest+1:]...)
	}
}

// openCodeUsageBucketForDay reads the bucket for (fingerprint, the local date of
// now), and the ledger's generation.
func openCodeUsageBucketForDay(fingerprint string, now time.Time) (openCodeUsageBucket, bool, cliUsageGeneration) {
	today, _ := openCodeLocalDay(now)
	openCodeUsageMu.Lock()
	ledger := readOpenCodeUsageLedger()
	openCodeUsageMu.Unlock()
	for _, b := range ledger.Buckets {
		if b.AccountFingerprint == fingerprint && b.LocalDate == today {
			return b, true, ledger.Generation
		}
	}
	return openCodeUsageBucket{}, false, ledger.Generation
}

// openCodeUsageMetrics turns today's bucket into the card's rows: tokens
// always, cost only when above zero — a local model or a subscription
// provider reports 0, and "$0.00" would read as free rather than unmetered. No
// bucket, no rows.
func openCodeUsageMetrics(bucket openCodeUsageBucket, ok bool, now time.Time) []cliAgentUsageMetric {
	if !ok || (bucket.tokens() <= 0 && bucket.CostUsd <= 0) {
		return nil
	}
	_, resetAt := openCodeLocalDay(now)
	observedAt := time.UnixMilli(bucket.ObservedAtMs).UTC().Format(time.RFC3339)
	reset := resetAt.Format(time.RFC3339)
	tokens := float64(bucket.tokens())
	metrics := []cliAgentUsageMetric{{
		Kind:       limitKindDaily,
		Label:      "Tokens today (agent runs)",
		Unit:       "tokens",
		Consumed:   &tokens,
		ResetAt:    reset,
		ObservedAt: observedAt,
	}}
	if bucket.CostUsd > 0 {
		cost := math.Round(bucket.CostUsd*10000) / 10000
		metrics = append(metrics, cliAgentUsageMetric{
			Kind:       limitKindDaily,
			Label:      "Cost today",
			Unit:       "usd",
			Consumed:   &cost,
			ResetAt:    reset,
			ObservedAt: observedAt,
		})
	}
	return metrics
}

/* --------------------------------------------------------------------------
   Frame parsing
   -------------------------------------------------------------------------- */

// openCodeUsageRun is one turn's in-memory accumulator. Parsing appends to it;
// nothing here does I/O. A nil run ignores everything, so a caller that could
// not arm still streams.
type openCodeUsageRun struct {
	id          string
	floorMs     int64
	fingerprint string
	executable  string
	dir         string

	mu    sync.Mutex
	steps []openCodeUsageStep
	// committed counts the leading steps already handed to the ledger.
	committed int
	sessionID string
	settled   atomic.Bool
	// persistedSession is set once the session id reached the armed debt.
	persistedSession atomic.Bool
}

// captureOpenCodeUsageLine reads one streamed line: a step-finish frame's
// usage joins the run, and the first session id any frame names is recorded on
// the run's debt — once per run, off the calling goroutine.
func captureOpenCodeUsageLine(run *openCodeUsageRun, line string) {
	if run == nil {
		return
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return
	}
	// Cheap pre-filter, so the stream does not decode every text delta: once
	// the session is known only a step frame carrying tokens can matter; until
	// then, any frame naming a session.
	run.mu.Lock()
	knownSession := run.sessionID != ""
	run.mu.Unlock()
	isStep := strings.Contains(line, "tokens") && strings.Contains(line, "step")
	if !isStep && (knownSession || !strings.Contains(line, "ession")) {
		return
	}
	step, sessionID, ok := parseOpenCodeUsageFrame(line, time.Now())
	if !isValidOpenCodeSessionID(sessionID) {
		sessionID = ""
	}
	run.mu.Lock()
	newSession := sessionID != "" && run.sessionID == ""
	if newSession {
		run.sessionID = sessionID
	}
	if ok {
		switch {
		case len(run.steps) < openCodeUsageMaxRunSteps:
			run.steps = append(run.steps, step)
		case run.committed >= len(run.steps):
			logOpenCodeUsageCapture("run_step_cap")
		default:
			last := &run.steps[len(run.steps)-1]
			last.Key = ""
			last.Input += step.Input
			last.Output += step.Output
			last.Reasoning += step.Reasoning
			last.CacheRead += step.CacheRead
			last.CacheWrite += step.CacheWrite
			last.Cost += step.Cost
			last.AtMs = max(last.AtMs, step.AtMs)
		}
	}
	run.mu.Unlock()
	if newSession {
		run.persistSessionIDAsync(sessionID)
	}
}

// captureOpenCodeUsageStream folds a buffered stdout (the smoke's) into run.
func captureOpenCodeUsageStream(stdout []byte, run *openCodeUsageRun) {
	if run == nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), openCodeNativeMaxFrameBytes)
	for scanner.Scan() {
		captureOpenCodeUsageLine(run, scanner.Text())
	}
}

// snapshot returns a copy of the run's steps and its session id.
func (run *openCodeUsageRun) snapshot() ([]openCodeUsageStep, string) {
	run.mu.Lock()
	defer run.mu.Unlock()
	return append([]openCodeUsageStep(nil), run.steps...), run.sessionID
}

// takeUncommitted returns the steps not yet handed to the ledger, marking them
// handed, and the run's session id. from is the handed count before the call,
// for untake.
func (run *openCodeUsageRun) takeUncommitted() (steps []openCodeUsageStep, sessionID string, from int) {
	run.mu.Lock()
	defer run.mu.Unlock()
	from = run.committed
	steps = append([]openCodeUsageStep(nil), run.steps[run.committed:]...)
	run.committed = len(run.steps)
	return steps, run.sessionID, from
}

// untake hands steps taken from `from` back, so a write the ledger refused
// (another agent process holding it, a failed rename) is retried by the run's
// next flush instead of lost.
func (run *openCodeUsageRun) untake(from int) {
	run.mu.Lock()
	if from < run.committed {
		run.committed = from
	}
	run.mu.Unlock()
}

// openCodeUsageFrame is the permissive subset of an event the capture reads.
// Numbers are RawMessage so a string-typed or absent field degrades to "no
// reading" rather than a decode failure.
type openCodeUsageFrame struct {
	Type      string          `json:"type"`
	SessionID string          `json:"sessionID"`
	SessionId string          `json:"sessionId"`
	Timestamp json.RawMessage `json:"timestamp"`
	Tokens    json.RawMessage `json:"tokens"`
	Cost      json.RawMessage `json:"cost"`
	Part      *struct {
		ID        string          `json:"id"`
		Type      string          `json:"type"`
		SessionID string          `json:"sessionID"`
		MessageID string          `json:"messageID"`
		Tokens    json.RawMessage `json:"tokens"`
		Cost      json.RawMessage `json:"cost"`
		Time      struct {
			End json.RawMessage `json:"end"`
		} `json:"time"`
	} `json:"part"`
}

// isOpenCodeStepFinishType matches the step-finish spellings OpenCode has
// used: `step_finish` (the event), `step-finish` (its part) and `step.finish`.
// isOpenCodeTerminalEventType answers a different question (did the TURN end)
// and rejects a tool-call step, which still spent tokens.
func isOpenCodeStepFinishType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "step_finish", "step-finish", "step.finish":
		return true
	}
	return false
}

// parseOpenCodeUsageFrame decodes one line. ok reports a step-finish frame
// with a valid usage reading; sessionID is any session id the frame names,
// whatever its type. A negative or non-finite value rejects the frame.
func parseOpenCodeUsageFrame(line string, capturedAt time.Time) (step openCodeUsageStep, sessionID string, ok bool) {
	var frame openCodeUsageFrame
	if json.Unmarshal([]byte(line), &frame) != nil {
		return openCodeUsageStep{}, "", false
	}
	partType, partID, messageID := "", "", ""
	tokensRaw, costRaw, endRaw := frame.Tokens, frame.Cost, json.RawMessage(nil)
	if frame.Part != nil {
		partType, partID, messageID = frame.Part.Type, frame.Part.ID, frame.Part.MessageID
		sessionID = frame.Part.SessionID
		if len(frame.Part.Tokens) > 0 {
			tokensRaw = frame.Part.Tokens
		}
		if len(frame.Part.Cost) > 0 {
			costRaw = frame.Part.Cost
		}
		endRaw = frame.Part.Time.End
	}
	sessionID = firstNonEmpty(sessionID, frame.SessionID, frame.SessionId)
	if !isOpenCodeStepFinishType(frame.Type) && !isOpenCodeStepFinishType(partType) {
		return openCodeUsageStep{}, sessionID, false
	}
	step, valid := openCodeUsageFromTokens(tokensRaw, costRaw)
	if !valid {
		logOpenCodeUsageCapture("frame_rejected")
		return openCodeUsageStep{}, sessionID, false
	}
	step.AtMs = capturedAt.UnixMilli()
	end, endOK := openCodeUsageMillis(endRaw)
	if !endOK {
		end, endOK = openCodeUsageMillis(frame.Timestamp)
	}
	if endOK {
		step.AtMs = end
	}
	switch {
	case partID != "":
		step.Key = openCodeUsageStepKey(sessionID, partID)
	case messageID != "" && endOK:
		step.Key = openCodeUsageStepKey(sessionID, messageID+"@"+strconv.FormatInt(end, 10))
	}
	return step, sessionID, true
}

// openCodeUsageFromTokens reads a tokens object — nested
// `{input, output, reasoning, cache: {read, write}}` or flat (`cacheRead`,
// `cache_read`, `inputTokens`, …) — and a cost. valid=false for a missing
// tokens object or any negative / non-finite value.
func openCodeUsageFromTokens(tokensRaw, costRaw json.RawMessage) (openCodeUsageStep, bool) {
	var tokens map[string]json.RawMessage
	if len(tokensRaw) == 0 || json.Unmarshal(tokensRaw, &tokens) != nil || tokens == nil {
		return openCodeUsageStep{}, false
	}
	var cache map[string]json.RawMessage
	if raw, ok := tokens["cache"]; ok {
		_ = json.Unmarshal(raw, &cache)
	}
	var step openCodeUsageStep
	valid := true
	read := func(m map[string]json.RawMessage, keys ...string) int64 {
		for _, k := range keys {
			raw, ok := m[k]
			if !ok {
				continue
			}
			n, ok := openCodeUsageNumber(raw)
			if !ok || n < 0 {
				valid = false
				return 0
			}
			return int64(math.Round(n))
		}
		return 0
	}
	step.Input = read(tokens, "input", "inputTokens", "input_tokens")
	step.Output = read(tokens, "output", "outputTokens", "output_tokens")
	step.Reasoning = read(tokens, "reasoning", "reasoningTokens", "reasoning_tokens")
	step.CacheRead = read(tokens, "cacheRead", "cache_read", "cacheReadTokens", "cache_read_tokens")
	step.CacheWrite = read(tokens, "cacheWrite", "cache_write", "cacheWriteTokens", "cache_write_tokens")
	if cache != nil {
		if step.CacheRead == 0 {
			step.CacheRead = read(cache, "read")
		}
		if step.CacheWrite == 0 {
			step.CacheWrite = read(cache, "write")
		}
	}
	if len(costRaw) > 0 && string(costRaw) != "null" {
		cost, ok := openCodeUsageNumber(costRaw)
		if !ok || cost < 0 {
			return openCodeUsageStep{}, false
		}
		step.Cost = cost
	}
	return step, valid
}

// openCodeUsageNumber reads a JSON number. A string, NaN or ±Inf is invalid.
func openCodeUsageNumber(raw json.RawMessage) (float64, bool) {
	var n float64
	if json.Unmarshal(raw, &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, false
	}
	return n, true
}

// openCodeUsageMillis reads a positive epoch-millisecond timestamp.
func openCodeUsageMillis(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	n, ok := openCodeUsageNumber(raw)
	if !ok || n <= 0 {
		return 0, false
	}
	return int64(n), true
}

// openCodeSessionIDPattern is what an OpenCode session id may look like before
// this file stores it or puts it on the export's argv (`ses_…` today): no
// leading dash an option parser would read as a flag, and no character cmd.exe
// treats as syntax when the binary is an npm `.cmd` shim. The id comes from the
// child's own stdout, so anything else is dropped rather than trusted.
var openCodeSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func isValidOpenCodeSessionID(id string) bool {
	return openCodeSessionIDPattern.MatchString(id)
}

// openCodeUsageStepKey hashes a step's identity: sessionID NUL id, 16 hex chars.
func openCodeUsageStepKey(sessionID, id string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + id))
	return hex.EncodeToString(sum[:8])
}

func logOpenCodeUsageCapture(label string) {
	fmt.Printf("%s[cli-usage] opencode capture: %s%s\n", colorCyan, label, colorReset)
}
