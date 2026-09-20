package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_claudecode_pending_run_test.go
   --------------------------------------------------------------------------
   The record exists for ONE window: the agent restarts (auto-update, crash,
   self-replace) between a Claude turn and the refresh that should report it.
   What is pinned here is that it survives exactly that, that every guard
   DISCARDS rather than mis-pays, that a settlement clears it, and that nothing
   but numbers and the account hash ever reaches the file.
   ------------------------------------------------------------------------ */

// pendingRunEnv isolates BOTH the record and the rate-limit cache the record's
// account scope is read from, and seeds the cache so a persist has an account
// to key on (see claudeUsagePendingRunAccounts).
func pendingRunEnv(t *testing.T, fingerprint string) string {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "pending_run.json")
	cache := filepath.Join(dir, "rl.json")
	t.Setenv(claudeUsagePendingRunEnv, record)
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	resetClaudeUsagePendingRun()
	t.Cleanup(resetClaudeUsagePendingRun)

	now := time.Now()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 10, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.Add(-time.Hour).UnixMilli(), usageKnown: true,
		},
	}, now.Add(-time.Hour), fingerprint, claudeRateLimitSourceStream)
	return record
}

func writePendingRunRecord(t *testing.T, path string, rec claudeUsagePendingRun) {
	t.Helper()
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeUsagePendingRun_RoundTrips(t *testing.T) {
	record := pendingRunEnv(t, "")
	baseline := time.Now().Add(-time.Minute).Truncate(time.Millisecond)

	claudeUsageRecordPendingRun(baseline)

	got, ok := loadClaudeUsagePendingRun("", time.Now())
	if !ok {
		t.Fatalf("record at %s was not readable back", record)
	}
	if !got.Equal(baseline) {
		t.Errorf("baseline = %v, want %v", got, baseline)
	}
}

// Every discard path: a record that cannot be trusted must read as "no debt",
// never as a debt with a wrong baseline — the latter would either spend an OAuth
// request for nothing or suppress the probe that was actually owed.
func TestClaudeUsagePendingRun_GuardsDiscardRatherThanMisPay(t *testing.T) {
	now := time.Now()
	baselineMs := now.Add(-time.Minute).UnixMilli()

	for name, tc := range map[string]struct {
		write       func(t *testing.T, path string)
		fingerprint string
	}{
		"absent file": {write: func(*testing.T, string) {}},
		"corrupt JSON": {write: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		"unknown schema": {write: func(t *testing.T, path string) {
			writePendingRunRecord(t, path, claudeUsagePendingRun{
				SchemaVersion:    claudeUsagePendingRunSchema + 1,
				OwedObservedAtMs: baselineMs, RecordedAtMs: now.UnixMilli(),
			})
		}},
		"another account": {
			write: func(t *testing.T, path string) {
				writePendingRunRecord(t, path, claudeUsagePendingRun{
					SchemaVersion: claudeUsagePendingRunSchema, AccountFingerprint: "someone-else",
					OwedObservedAtMs: baselineMs, RecordedAtMs: now.UnixMilli(),
				})
			},
			fingerprint: "us",
		},
		"past the max age": {write: func(t *testing.T, path string) {
			stale := now.Add(-claudeUsagePendingRunMaxAge - time.Minute)
			writePendingRunRecord(t, path, claudeUsagePendingRun{
				SchemaVersion:    claudeUsagePendingRunSchema,
				OwedObservedAtMs: stale.UnixMilli(), RecordedAtMs: stale.UnixMilli(),
			})
		}},
		"no baseline": {write: func(t *testing.T, path string) {
			writePendingRunRecord(t, path, claudeUsagePendingRun{
				SchemaVersion: claudeUsagePendingRunSchema, RecordedAtMs: now.UnixMilli(),
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			record := pendingRunEnv(t, tc.fingerprint)
			tc.write(t, record)
			if got, ok := loadClaudeUsagePendingRun(tc.fingerprint, now); ok {
				t.Fatalf("an untrustworthy record was accepted as a debt at %v", got)
			}
		})
	}
}

// A record written just inside the max age is still payable — the boundary must
// not be so tight that an ordinary overnight sleep discards a real debt.
func TestClaudeUsagePendingRun_JustInsideTheMaxAgeIsStillPayable(t *testing.T) {
	record := pendingRunEnv(t, "")
	now := time.Now()
	recorded := now.Add(-claudeUsagePendingRunMaxAge + time.Minute)
	writePendingRunRecord(t, record, claudeUsagePendingRun{
		SchemaVersion:    claudeUsagePendingRunSchema,
		OwedObservedAtMs: recorded.UnixMilli(),
		RecordedAtMs:     recorded.UnixMilli(),
	})
	if _, ok := loadClaudeUsagePendingRun("", now); !ok {
		t.Fatal("a record one minute inside the max age must still be payable")
	}
}

// Retention: numbers and the account HASH, nothing else. The same rule
// claudeRateLimitSnapshot obeys — no token, no path, no email.
func TestClaudeUsagePendingRun_FileHoldsOnlyNumbersAndTheAccountHash(t *testing.T) {
	record := pendingRunEnv(t, fingerprintAccount("claudeCode", "someone@example.com"))
	claudeUsageRecordPendingRun(time.Now())

	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("record is not JSON: %v", err)
	}
	for key, value := range decoded {
		if key == "accountFingerprint" {
			continue
		}
		if key == "accountFingerprints" {
			// The alternate accounts are the same hashes, in a list.
			for _, alt := range value.([]any) {
				if _, isString := alt.(string); !isString {
					t.Errorf("accountFingerprints holds a non-hash %v", alt)
				}
			}
			continue
		}
		if _, numeric := value.(float64); !numeric {
			t.Errorf("field %q = %v is not numeric; only the account hash may be a string", key, value)
		}
	}
	text := string(raw)
	for _, banned := range []string{"example.com", "someone", os.Getenv("HOME"), "accessToken", "Bearer"} {
		if banned == "" {
			continue
		}
		if strings.Contains(text, banned) {
			t.Errorf("record leaked %q: %s", banned, text)
		}
	}
}

// Settlement clears the file, and a persist that was already in flight when the
// settlement landed must not put it back — a resurrected debt is permanent, and
// costs one OAuth request on every process start until the max age expires it.
func TestClaudeUsagePendingRun_SettlementIsNotResurrected(t *testing.T) {
	record := pendingRunEnv(t, "")
	baseline := time.Now().Add(-time.Minute)

	claudeUsageRecordPendingRun(baseline)
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("record was not written: %v", err)
	}

	clearClaudeUsagePendingRun(baseline)
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("settlement did not remove the record (err=%v)", err)
	}

	// A late writer for the same (or an older) baseline is refused.
	claudeUsageRecordPendingRun(baseline)
	claudeUsageRecordPendingRun(baseline.Add(-time.Second))
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("a settled debt was resurrected by a late writer")
	}

	// A genuinely NEWER run is a new debt and must still be recordable.
	claudeUsageRecordPendingRun(baseline.Add(time.Minute))
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("a newer run could not record its own debt: %v", err)
	}
}

// Hydration is scoped to the account — a record for someone else is not this
// gather's to pay — but a mismatch is not an ANSWER: the gather's identity comes
// from a bounded credential read that can fail, so the next one gets to try
// again. (What the latch does close on is covered by the two tests below.)
func TestClaudeUsageHydratePendingRun_ScopedToTheAccountAndRetriedAfterAMismatch(t *testing.T) {
	record := pendingRunEnv(t, "")
	resetClaudeUsageProbeGate()
	t.Cleanup(resetClaudeUsageProbeGate)

	now := time.Now()
	baseline := now.Add(-time.Minute).Truncate(time.Millisecond)
	writePendingRunRecord(t, record, claudeUsagePendingRun{
		SchemaVersion: claudeUsagePendingRunSchema, AccountFingerprint: "acct-a",
		OwedObservedAtMs: baseline.UnixMilli(), RecordedAtMs: now.UnixMilli(),
	})

	claudeUsageHydratePendingRun("acct-b", now)
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Fatalf("another account's debt was inherited (%v)", owed)
	}

	// That refused attempt did NOT spend the latch: had it been a Keychain read
	// that timed out rather than a genuinely different account, latching would
	// have dropped a real debt for the life of the process.
	claudeUsageHydratePendingRun("acct-a", now)
	if owed := claudeUsageProbe.owedObservation(); !owed.Equal(baseline) {
		t.Fatalf("debt after the identity resolved = %v, want %v", owed, baseline)
	}
	claudeUsageProbe.settleOwed(baseline)

	// After a restart (the reset seam clears the latch AND the record), a record
	// for the right account is inherited.
	resetClaudeUsagePendingRun()
	writePendingRunRecord(t, record, claudeUsagePendingRun{
		SchemaVersion: claudeUsagePendingRunSchema, AccountFingerprint: "acct-a",
		OwedObservedAtMs: baseline.UnixMilli(), RecordedAtMs: now.UnixMilli(),
	})
	claudeUsageHydratePendingRun("acct-a", now)
	if owed := claudeUsageProbe.owedObservation(); !owed.Equal(baseline) {
		t.Fatalf("inherited debt = %v, want %v", owed, baseline)
	}
}

// A read-only (or missing) data dir must degrade to exactly the pre-change
// behaviour: no debt persisted, no error, no panic.
func TestClaudeUsagePendingRun_UnwritablePathIsSilent(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "rl.json")
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", cache)
	// A path whose parent is a FILE — every write below it fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(claudeUsagePendingRunEnv, filepath.Join(blocker, "pending_run.json"))
	resetClaudeUsagePendingRun()
	t.Cleanup(resetClaudeUsagePendingRun)

	now := time.Now()
	mergeClaudeRateLimitCacheFromSource(cache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {UsedPercentage: 1, ObservedAtMs: now.UnixMilli(), usageKnown: true},
	}, now, "", claudeRateLimitSourceStream)

	claudeUsageRecordPendingRun(now)
	if _, ok := loadClaudeUsagePendingRun("", now); ok {
		t.Fatal("a record appeared on an unwritable path")
	}
	// And the next call is not wedged by the failed one.
	claudeUsageRecordPendingRun(now.Add(time.Second))
}

// A device that has never observed anything has no stale row to rescue, so the
// persist is skipped rather than writing a record with a guessed account.
func TestClaudeUsagePendingRun_SkippedWithNoRateLimitCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", filepath.Join(dir, "rl.json"))
	t.Setenv(claudeUsagePendingRunEnv, filepath.Join(dir, "pending_run.json"))
	resetClaudeUsagePendingRun()
	t.Cleanup(resetClaudeUsagePendingRun)

	claudeUsageRecordPendingRun(time.Now())
	if _, ok := loadClaudeUsagePendingRun("", time.Now()); ok {
		t.Fatal("a debt was persisted for a device with no cached reading at all")
	}
}

// The tmp+rename write must leave no debris behind on the happy path.
func TestClaudeUsagePendingRun_LeavesNoTempFiles(t *testing.T) {
	record := pendingRunEnv(t, "")
	claudeUsageRecordPendingRun(time.Now())

	entries, err := os.ReadDir(filepath.Dir(record))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp.") {
			t.Errorf("left a temp file behind: %s", entry.Name())
		}
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("the record itself is missing: %v", err)
	}
}

// Once the record HAS been answered, the latch closes: a debt this process
// already settled must not be hydrated a second time from a record a concurrent
// writer left behind, which would resurrect it and buy an OAuth request for a
// reading already taken.
func TestClaudeUsageHydratePendingRun_DoesNotRehydrateASettledDebt(t *testing.T) {
	account := fingerprintAccount("claudeCode", "someone@example.com")
	record := pendingRunEnv(t, account)
	resetClaudeUsageProbeGate()
	t.Cleanup(resetClaudeUsageProbeGate)

	now := time.Now()
	baseline := now.Add(-time.Minute).Truncate(time.Millisecond)
	writeRecord := func() {
		writePendingRunRecord(t, record, claudeUsagePendingRun{
			SchemaVersion: claudeUsagePendingRunSchema, AccountFingerprint: account,
			OwedObservedAtMs: baseline.UnixMilli(), RecordedAtMs: now.UnixMilli(),
		})
	}

	writeRecord()
	claudeUsageHydratePendingRun(account, now)
	owed := claudeUsageProbe.owedObservation()
	if owed.UnixMilli() != baseline.UnixMilli() {
		t.Fatalf("inherited debt = %v, want the persisted baseline %v", owed, baseline)
	}

	claudeUsageProbe.settleOwed(owed)
	writeRecord()
	claudeUsageHydratePendingRun(account, now)
	if owed := claudeUsageProbe.owedObservation(); !owed.IsZero() {
		t.Errorf("a settled debt was hydrated a second time (%v)", owed)
	}
}

// A record that has aged out is a permanent answer no matter whose it is: the
// age and shape guards run BEFORE the fingerprint, so an expired record left by
// another account cannot hold the latch open and buy a file read per gather for
// the rest of the process's life.
func TestClaudeUsageHydratePendingRun_AnExpiredRecordClosesTheLatch(t *testing.T) {
	record := pendingRunEnv(t, "us")
	now := time.Now()
	stale := now.Add(-claudeUsagePendingRunMaxAge - time.Minute)
	writePendingRunRecord(t, record, claudeUsagePendingRun{
		SchemaVersion: claudeUsagePendingRunSchema, AccountFingerprint: "someone-else",
		OwedObservedAtMs: stale.UnixMilli(), RecordedAtMs: stale.UnixMilli(),
	})

	claudeUsageHydratePendingRun("us", now)
	if _, _, settled := claudeUsagePendingRunLoad("us", now); !settled {
		t.Fatal("an expired record was treated as a question still open")
	}
	if !claudeUsagePendingRunState.hydrated {
		t.Error("the latch stayed open on a record that can never be payable")
	}
}

// A box with two agent channels shares ONE ~/.claude/settings.json, so the
// channel that lost the status-line hook can have NO cache of its own and show
// rows exclusively from the pinned one. Reading the account from the local path
// alone would skip the persist there, and the pre-smoke pinned rows would go on
// suppressing the probe after the update — the dual-channel form of the exact
// failure the record exists to survive.
func TestClaudeUsagePendingRun_ScopedFromTheCachePinnedByTheInstalledHook(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "pending_run.json")
	ownCache := filepath.Join(dir, "own", "rl.json") // never written: this channel lost the hook
	pinnedCache := filepath.Join(dir, "pinned", "rl.json")
	configDir := t.TempDir()
	t.Setenv(claudeUsagePendingRunEnv, record)
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", ownCache)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	resetClaudeUsagePendingRun()
	t.Cleanup(resetClaudeUsagePendingRun)

	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinnedCache) +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})

	now := time.Now()
	mergeClaudeRateLimitCacheFromSource(pinnedCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 42, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.Add(-time.Hour).UnixMilli(), usageKnown: true,
		},
	}, now.Add(-time.Hour), "acct-pinned", claudeRateLimitSourceStream)

	baseline := now.Truncate(time.Millisecond)
	claudeUsageRecordPendingRun(baseline)

	got, ok := loadClaudeUsagePendingRun("acct-pinned", now)
	if !ok {
		t.Fatal("no payable debt: the persist read only this channel's own (absent) cache")
	}
	if !got.Equal(baseline) {
		t.Errorf("baseline=%s, want %s", got, baseline)
	}
	if _, ok := loadClaudeUsagePendingRun("someone-else", now); ok {
		t.Error("the record is not scoped to the pinned cache's account")
	}
}

// With both caches present, EVERY account found is payable — including an older
// cache's. Picking one by timestamp guesses which account the next gather will
// display, and after an account switch the newest cache can belong to the
// account the card is no longer showing; hydration would then reject the record
// and the fresh-looking rows would suppress the probe. An unrelated account is
// still refused.
func TestClaudeUsagePendingRun_ScopedToEveryAccountAcrossPaths(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "pending_run.json")
	ownCache := filepath.Join(dir, "own", "rl.json")
	pinnedCache := filepath.Join(dir, "pinned", "rl.json")
	configDir := t.TempDir()
	t.Setenv(claudeUsagePendingRunEnv, record)
	t.Setenv("AIEXPEDITE_CLAUDE_RL_CACHE", ownCache)
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	resetClaudeUsagePendingRun()
	t.Cleanup(resetClaudeUsagePendingRun)

	helperWriteJSON(t, filepath.Join(configDir, "settings.json"), map[string]any{
		"statusLine": map[string]any{
			"type": "command",
			"command": "AIEXPEDITE_CLAUDE_RL_CACHE=" + posixSingleQuote(pinnedCache) +
				" '/opt/aiexpedite/aiexpedite-terminal' " + statusLineHookArg,
		},
	})

	now := time.Now()
	// The account the card still displays (older cache, but inside the TTL)...
	mergeClaudeRateLimitCacheFromSource(ownCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 10, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.Add(-10 * time.Minute).UnixMilli(), usageKnown: true,
		},
	}, now.Add(-10*time.Minute), "acct-displayed", claudeRateLimitSourceStream)
	// ...and the other channel's, which happens to carry a newer observation.
	mergeClaudeRateLimitCacheFromSource(pinnedCache, map[string]claudeRateLimitBucket{
		claudeWindowFiveHour: {
			UsedPercentage: 42, ResetsAtMs: now.Add(time.Hour).UnixMilli(),
			ObservedAtMs: now.Add(-5 * time.Minute).UnixMilli(), usageKnown: true,
		},
	}, now.Add(-5*time.Minute), "acct-other", claudeRateLimitSourceStream)

	baseline := now.Truncate(time.Millisecond)
	claudeUsageRecordPendingRun(baseline)

	for _, account := range []string{"acct-other", "acct-displayed"} {
		got, ok := loadClaudeUsagePendingRun(account, now)
		if !ok {
			t.Errorf("the debt is not payable under %q — a gather for it would leave the card stale", account)
			continue
		}
		if !got.Equal(baseline) {
			t.Errorf("baseline under %q = %s, want %s", account, got, baseline)
		}
	}
	if _, ok := loadClaudeUsagePendingRun("acct-unrelated", now); ok {
		t.Error("an account with no cache on this device may not pay the debt")
	}
}

// Coalescing is by BASELINE: the trailing goroutine's repeat of what the smoke
// already wrote costs no second write, but ANY advance lands immediately —
// including one inside the same second. A time window here would leave the older
// baseline durable, and an observation taken between the two turns would then
// settle a hydrated debt that predates the second one.
func TestClaudeUsagePendingRun_CoalescesRepeatsButNeverHoldsBackAnAdvance(t *testing.T) {
	record := pendingRunEnv(t, "")
	first := time.Now().Truncate(time.Millisecond)

	claudeUsageRecordPendingRun(first)
	info, err := os.Stat(record)
	if err != nil {
		t.Fatal(err)
	}
	firstWrite := info.ModTime()

	// The repeat (and anything older) writes nothing at all.
	claudeUsageRecordPendingRun(first)
	claudeUsageRecordPendingRun(first.Add(-time.Minute))
	if after, err := os.Stat(record); err != nil {
		t.Fatal(err)
	} else if !after.ModTime().Equal(firstWrite) {
		t.Error("a repeated baseline rewrote the record")
	}

	// A second turn 50ms later is durable straight away.
	second := first.Add(50 * time.Millisecond)
	claudeUsageRecordPendingRun(second)
	got, ok := loadClaudeUsagePendingRun("", time.Now())
	if !ok {
		t.Fatal("the record disappeared")
	}
	if !got.Equal(second) {
		t.Errorf("durable baseline = %v, want the newest turn's %v — a coalesce window held it back", got, second)
	}
}

// A settlement clears only what it actually covers. If a newer run persists its
// own debt in the gap between a settlement reading the gate and clearing the
// file, deleting that record would drop a real debt just as the smoke hands the
// process over to an update.
func TestClaudeUsagePendingRun_AnOlderSettlementKeepsANewerRecord(t *testing.T) {
	record := pendingRunEnv(t, "")
	now := time.Now()
	older := now.Add(-time.Minute).Truncate(time.Millisecond)
	newer := now.Truncate(time.Millisecond)

	writePendingRunRecord(t, record, claudeUsagePendingRun{
		SchemaVersion: claudeUsagePendingRunSchema, AccountFingerprint: "",
		OwedObservedAtMs: newer.UnixMilli(), RecordedAtMs: now.UnixMilli(),
	})

	clearClaudeUsagePendingRun(older)

	got, ok := loadClaudeUsagePendingRun("", now)
	if !ok {
		t.Fatal("a settlement for an older run deleted the newer run's debt")
	}
	if !got.Equal(newer) {
		t.Errorf("surviving baseline = %v, want %v", got, newer)
	}

	// Its own settlement does clear it.
	clearClaudeUsagePendingRun(newer)
	if _, ok := loadClaudeUsagePendingRun("", now); ok {
		t.Error("the covering settlement left the record behind")
	}
}
