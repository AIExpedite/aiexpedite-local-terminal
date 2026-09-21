package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   cliagent_usage_grok_post_update_freshness_test.go
   --------------------------------------------------------------------------
   The acceptance criterion "usage freshens after upgrade", proved across the
   smoke and usage layers together:

     1. the smoke cooldown is keyed on the binary stamp, so an upgrade makes
        the post-update smoke EXECUTE rather than replay;
     2. the smoke child's `billing: fetched credits config` record, written
        into the isolated home, is merged into the persistent home through
        persistGrokManagedBillingSnapshot before the isolated home is removed;
     3. the next signed refresh resolves Grok's observation as the NEWEST of
        the merged smoke snapshot, the persistent log tail (where a direct run
        writes) and the live billing cache — so latestObservedAt strictly
        advances for a smoke, a direct (ACP) run and a terminal (session) run
        alike.

   The terminal (session_start) transport is exercised end to end by
   TestSessionLifecycle_GrokNoToolsSmokeSurvivesUpdateAndSignedRefresh in
   session_integration_test.go; this file covers the __cli_smoke__ transport
   and the direct-run source at the usage layer.
   ------------------------------------------------------------------------ */

// writeGrokSmokeBillingRecord is what a smoke child leaves behind: one
// `billing: fetched credits config` record appended to the ISOLATED home's
// unified log (the home the launch env names), after the producer identity
// the agent seeded there. Carries the same secret/raw-config sentinels the
// session mock uses so a leak into the published usage would be visible.
func writeGrokSmokeBillingRecord(t *testing.T, launch grokSmokeLaunch, observedAt time.Time) {
	t.Helper()
	isolated := ""
	for _, kv := range launch.Env {
		if v, ok := strings.CutPrefix(kv, "GROK_HOME="); ok {
			isolated = v
		}
	}
	if isolated == "" {
		t.Fatal("launch env names no GROK_HOME")
	}
	appendGrokBillingRecordAt(t, isolated, observedAt)
}

// appendGrokBillingRecordAt appends one allowlisted billing record to a home's
// unified log, in the exact shape Grok 1.0 writes.
func appendGrokBillingRecordAt(t *testing.T, home string, observedAt time.Time) {
	t.Helper()
	path := grokBillingLogPath(home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	line := fmt.Sprintf(`{"ts":%q,"msg":"billing: fetched credits config","credential":"credential-sentinel","prompt":"prompt-sentinel","ctx":{"config":{"creditUsagePercent":%d,"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":%q,"end":%q},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"rawConfig":"raw-config-sentinel"},"subscriptionTier":"SuperGrok"}}`+"\n",
		observedAt.UTC().Format(time.RFC3339Nano), observedAt.Second()%100,
		observedAt.Add(-time.Hour).UTC().Format(time.RFC3339Nano), observedAt.Add(7*24*time.Hour).UTC().Format(time.RFC3339Nano))
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

// grokObservedAtViaSignedRefresh runs the production Grok parser against the
// persistent home and then the signed refresh normalizer, returning the
// observation the card would publish. Fails when the reading is unobservable
// or the normalized payload leaks a sentinel.
func grokObservedAtViaSignedRefresh(t *testing.T, refreshID string) time.Time {
	t.Helper()
	usage, ok := grokUsageParser{}.Parse("", detectedCLIAgent{Detected: true, Path: "grok", Version: "grok"}, time.Now())
	if !ok || usage == nil || len(usage.Metrics) != 1 {
		t.Fatalf("grok usage parse = %+v ok=%t", usage, ok)
	}
	receipt, normalized, normalizedErrs, err := prepareCLIUsageRefreshResult(
		"signed-refresh-secret", refreshID, time.Now().UnixMilli(), true, []cliAgentUsage{*usage}, nil)
	if err != nil || receipt == "" || len(normalizedErrs) != 0 || len(normalized) != 1 || len(normalized[0].Metrics) != 1 {
		t.Fatalf("signed refresh: receipt=%q usage=%+v errors=%+v err=%v", receipt, normalized, normalizedErrs, err)
	}
	encoded, _ := json.Marshal(normalized[0])
	for _, secret := range []string{"credential-sentinel", "prompt-sentinel", "raw-config-sentinel"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("signed refresh leaked %q: %s", secret, encoded)
		}
	}
	metric := normalized[0].Metrics[0]
	if metric.Unknown || metric.ObservedAt == "" {
		t.Fatalf("grok reading is unobservable: %+v", metric)
	}
	observed, err := time.Parse(time.RFC3339Nano, metric.ObservedAt)
	if err != nil {
		t.Fatalf("observedAt %q: %v", metric.ObservedAt, err)
	}
	return observed
}

func TestGrokUsage_FreshensAfterUpgradeThroughCliSmokeAndDirectRun(t *testing.T) {
	persistent := grokSmokeEnv(t)
	path := stubGrokBinary(t)
	stubGrokSmokePath(t, path)
	seedProbeVersion(t, path, "grok 1.0.5")

	// Pre-update state: a reading the previous day, attributed to the login,
	// plus a live-cache reading from the same time.
	dayBefore := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatal(err)
	}
	appendGrokBillingRecordAt(t, persistent, dayBefore)
	account, _ := readGrokAccountAndPlan(persistent)
	fingerprint := fingerprintAccount("grok", account)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: dayBefore, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 5, HasUsedPercent: true,
	}, fingerprint))
	seeded := grokObservedAtViaSignedRefresh(t, "refresh-0")
	if !seeded.Equal(dayBefore) {
		t.Fatalf("seeded observation = %s, want %s", seeded, dayBefore)
	}

	// The smoke child writes a record observed NOW into its isolated home.
	var isolatedHomes []string
	calls, launches := stubGrokSmokeExec(t, func(ctx context.Context, launch grokSmokeLaunch) ([]byte, []byte, error) {
		for _, kv := range launch.Env {
			if v, ok := strings.CutPrefix(kv, "GROK_HOME="); ok {
				isolatedHomes = append(isolatedHomes, v)
			}
		}
		// Second precision: the signed payload renders RFC3339 seconds, so
		// cross a boundary between observations rather than racing one.
		writeGrokSmokeBillingRecord(t, launch, time.Now().Truncate(time.Second))
		return grokSuccessFrames(grokMarkerFromLaunch(t, launch)), nil, nil
	})

	// Pre-update smoke.
	pre, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || pre.Status != cliSmokeStatusSuccess || !pre.MarkerMatched || pre.Version != "grok 1.0.5" {
		t.Fatalf("pre-update smoke = %+v replayed=%t", pre, replayed)
	}
	preObserved := grokObservedAtViaSignedRefresh(t, "refresh-1")
	if !preObserved.After(seeded) {
		t.Fatalf("pre-update smoke did not advance the observation: %s !> %s", preObserved, seeded)
	}

	// The upgrade: a different binary (mtime/size) reporting a new version.
	// Cross a second boundary so the post-update record is strictly newer.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))
	if err := os.WriteFile(path, []byte("stub-upgraded"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedProbeVersion(t, path, "grok 1.0.13")

	post, replayed := runCLISmoke(context.Background(), "grok")
	if replayed || post.Status != cliSmokeStatusSuccess || !post.MarkerMatched || post.Version != "grok 1.0.13" {
		t.Fatalf("post-update smoke replayed or failed: %+v replayed=%t", post, replayed)
	}
	if *calls != 2 {
		t.Fatalf("expected both smokes to execute, got %d children", *calls)
	}
	postObserved := grokObservedAtViaSignedRefresh(t, "refresh-2")
	if !postObserved.After(preObserved) {
		t.Fatalf("post-update usage freshness did not advance: pre=%s post=%s", preObserved, postObserved)
	}

	// The isolated homes are gone; the prompt files are gone.
	for _, home := range isolatedHomes {
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Errorf("isolated home %q survived (stat err = %v)", home, err)
		}
	}
	for _, launch := range *launches {
		if _, err := os.Stat(launch.PromptFile); !os.IsNotExist(err) {
			t.Errorf("prompt file %q survived (stat err = %v)", launch.PromptFile, err)
		}
	}

	// A DIRECT (ACP / PTY) run: the CLI writes its record into the persistent
	// log under the identity the session start appended. The newest source
	// wins again, so the direct run advances the same observation.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))
	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatal(err)
	}
	directAt := time.Now().Truncate(time.Second)
	appendGrokBillingRecordAt(t, persistent, directAt)
	directObserved := grokObservedAtViaSignedRefresh(t, "refresh-3")
	if !directObserved.After(postObserved) || !directObserved.Equal(directAt) {
		t.Fatalf("direct run did not advance the observation: post=%s direct=%s (record %s)", postObserved, directObserved, directAt)
	}

	// And a still-newer live reading wins over both.
	liveAt := directAt.Add(time.Minute)
	saveGrokBillingLive(grokBillingLiveFromSnapshot(grokBillingSnapshot{
		ObservedAt: liveAt, PeriodType: "USAGE_PERIOD_TYPE_WEEKLY", UsedPercent: 9, HasUsedPercent: true,
	}, fingerprint))
	if liveObserved := grokObservedAtViaSignedRefresh(t, "refresh-4"); !liveObserved.Equal(liveAt) {
		t.Fatalf("live reading did not win: got %s, want %s", liveObserved, liveAt)
	}
}

// A contested producer still refuses the merge: when the child could have
// billed an account the copied login does not name, nothing is written into
// the provider-owned log — the observation stays where it was rather than
// publishing one account's spend as another's.
func TestGrokUsage_ContestedSmokeProducerNeverMergesItsRecord(t *testing.T) {
	persistent := grokSmokeEnv(t)
	dayBefore := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := appendGrokBillingIdentity(persistent); err != nil {
		t.Fatal(err)
	}
	appendGrokBillingRecordAt(t, persistent, dayBefore)

	isolated, err := setupIsolatedGrokSmokeHomeFrom(persistent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeIsolatedGrokHome(isolated) })
	appendGrokBillingRecordAt(t, isolated, time.Now().Truncate(time.Second))

	outcome, err := persistGrokManagedBillingSnapshot(isolated, persistent, true)
	if err != nil || outcome != grokManagedBillingContestedProducer {
		t.Fatalf("contested merge = %s, %v; want contested-producer and no error", outcome, err)
	}
	if observed := grokObservedAtViaSignedRefresh(t, "refresh-contested"); !observed.Equal(dayBefore) {
		t.Fatalf("contested smoke advanced the observation to %s", observed)
	}

	// The same child, uncontested, merges — proving the refusal above was the
	// producer verdict and not a broken fixture.
	outcome, err = persistGrokManagedBillingSnapshot(isolated, persistent, false)
	if err != nil || outcome != grokManagedBillingPersisted {
		t.Fatalf("uncontested merge = %s, %v; want persisted", outcome, err)
	}
	if observed := grokObservedAtViaSignedRefresh(t, "refresh-merged"); !observed.After(dayBefore) {
		t.Fatalf("uncontested smoke did not advance the observation: %s", observed)
	}
}
