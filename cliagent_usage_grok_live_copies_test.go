package main

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// The device whose card read "10 hours old": every Grok run there is an ACP
// session on an isolated COPY of the login, so the copies kept renewing their
// access tokens while the real home's expired. A Refresh then found the real
// home's token expired, could not renew (a copy was live), waited out the
// whole probe budget for a gap that never came, and reported a transport error
// for a login that was fine. These pin the two halves of the fix.

// TestProbeGrokBillingLive_PresentsAFreshTokenHeldByALiveCopy: an expired real
// home is not the end of the road when a live session's copy of the same login
// holds a renewed token — the probe presents that one, and renews nothing.
func TestProbeGrokBillingLive_PresentsAFreshTokenHeldByALiveCopy(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "stale-real", "dan@example.com", now.Add(-time.Minute))

	copyHome := t.TempDir()
	writeGrokAuth(t, copyHome, "fresh-copy", "dan@example.com", now.Add(5*time.Hour))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	calls := grokBillingServer(t, func(auth string) (int, string) {
		if auth != "Bearer fresh-copy" {
			return http.StatusUnauthorized, `{}`
		}
		return http.StatusOK, grokFixtureBody(now.Add(72 * time.Hour))
	})

	started := time.Now()
	if got := probeGrokBillingLive(context.Background(), "grok", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok from the live copy's token", got)
	}
	if elapsed := time.Since(started); elapsed > grokLoginRenewGap {
		t.Errorf("probe took %s: it waited for a renewal gap it never needed", elapsed)
	}
	if *calls != 1 {
		t.Errorf("requests=%d, want exactly one with the copy's token", *calls)
	}
	if _, err := os.Stat(os.Getenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE")); err != nil {
		t.Errorf("reading not cached: %v", err)
	}
}

// TestProbeGrokBillingLive_LoginBusyIsBoundedAndNamed: when every token is
// stale and a copy is live, the probe must neither renew nor spend its whole
// budget waiting — it gives up after grokLoginRenewGap and says the login is
// busy, not that the network failed.
func TestProbeGrokBillingLive_LoginBusyIsBoundedAndNamed(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "stale-real", "dan@example.com", now.Add(-time.Minute))
	renewals := 0
	runGrokLoginRenewal = func(context.Context, string, string) { renewals++ }

	copyHome := t.TempDir()
	writeGrokAuth(t, copyHome, "stale-copy", "dan@example.com", now.Add(-time.Minute))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	grokBillingServer(t, func(string) (int, string) { return http.StatusUnauthorized, `{}` })

	// A budget far longer than the gap: the probe must not consume it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	got := probeGrokBillingLive(ctx, "grok", time.Now)
	elapsed := time.Since(started)
	if got != grokLiveOutcomeLoginBusy {
		t.Fatalf("outcome=%q, want login_busy", got)
	}
	if renewals != 0 {
		t.Errorf("renewals=%d, want none while a copy of the login is live", renewals)
	}
	if elapsed < grokLoginRenewGap || elapsed > grokLoginRenewGap+3*time.Second {
		t.Errorf("probe took %s, want about one gap (%s): the wait is bounded by the gap, not the budget", elapsed, grokLoginRenewGap)
	}
	if _, err := os.Stat(os.Getenv("AIEXPEDITE_GROK_BILLING_LIVE_CACHE")); !os.IsNotExist(err) {
		t.Errorf("nothing may be cached when no token worked (stat err=%v)", err)
	}
}

// TestGrokFreshestPresentedToken_OnlyTheSameAccountsCopies: a copy holding a
// later expiry for ANOTHER account never lends its token; a copy for the same
// account does, and only when it is actually later.
func TestGrokFreshestPresentedToken_OnlyTheSameAccountsCopies(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuth(t, home, "real", "dan@example.com", now.Add(time.Hour))
	fingerprint := grokAccountFingerprintFor(home)

	other := t.TempDir()
	writeGrokAuth(t, other, "other-account", "someone@example.com", now.Add(9*time.Hour))
	older := t.TempDir()
	writeGrokAuth(t, older, "older-copy", "dan@example.com", now.Add(30*time.Minute))
	newer := t.TempDir()
	writeGrokAuth(t, newer, "newer-copy", "dan@example.com", now.Add(3*time.Hour))
	for _, dir := range []string{other, older, newer} {
		grokLogin.acquireCopy(dir)
		t.Cleanup(func() { grokLogin.releaseCopy(dir) })
	}

	token, expiresAt, hasExpiry := grokFreshestPresentedToken(home, fingerprint, now)
	if token != "newer-copy" || !hasExpiry {
		t.Fatalf("token=%q hasExpiry=%v, want the same account's newest copy", token, hasExpiry)
	}
	if !expiresAt.After(now.Add(2 * time.Hour)) {
		t.Errorf("expiresAt=%s, want the newer copy's expiry", expiresAt)
	}

	// Without any live copy the real home's own token is presented unchanged.
	for _, dir := range []string{other, older, newer} {
		grokLogin.releaseCopy(dir)
	}
	if token, _, _ := grokFreshestPresentedToken(home, fingerprint, now); token != "real" {
		t.Errorf("token=%q, want the real home's token once no copy is live", token)
	}
}
