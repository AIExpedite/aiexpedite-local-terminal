package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The computer that needed `grok login` every six hours: every Grok run there
// used a copy of the login, the first copy to hit the six-hour mark rotated
// the refresh token and was deleted, and the real home was left holding a
// token xAI revoked two minutes later. These pin the newest-wins rule that
// keeps one credential alive across the real home and every live copy.

// writeGrokAuthMinted writes a scoped auth file that carries the CLI's
// `create_time` stamp, which is what orders credentials of one account.
func writeGrokAuthMinted(t *testing.T, home, key, email string, minted, expires time.Time) {
	t.Helper()
	auth := map[string]any{
		grokExactOIDCScope: map[string]any{
			"key":           key,
			"email":         email,
			"user_id":       "user-" + email,
			"refresh_token": "refresh-for-" + key,
			"create_time":   minted.UTC().Format(time.RFC3339Nano),
			"expires_at":    expires.UTC().Format(time.RFC3339Nano),
		},
	}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func grokKeyIn(t *testing.T, home string) string {
	t.Helper()
	token, _, _ := grokPresentedToken(home)
	return token
}

func resetGrokKeeper(t *testing.T) {
	t.Helper()
	grokKeeper.mu.Lock()
	grokKeeper.lastRenewTry, grokKeeper.lastRenewFrom, grokKeeper.lastSweep = time.Time{}, time.Time{}, time.Time{}
	grokKeeper.mu.Unlock()
}

// TestReconcileGrokLogin_WritesASessionsRenewalBackToTheRealHome: a live copy
// that refreshed on its own holds the only live credential; the real home
// must receive it, and a copy of ANOTHER account must never be mixed in even
// when it is newer.
func TestReconcileGrokLogin_WritesASessionsRenewalBackToTheRealHome(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))

	renewedCopy := t.TempDir()
	writeGrokAuthMinted(t, renewedCopy, "copy-new", "dan@example.com", now.Add(-time.Minute), now.Add(6*time.Hour))
	otherAccount := t.TempDir()
	writeGrokAuthMinted(t, otherAccount, "other-newest", "someone@example.com", now, now.Add(6*time.Hour))
	for _, dir := range []string{renewedCopy, otherAccount} {
		grokLogin.acquireCopy(dir)
		t.Cleanup(func() { grokLogin.releaseCopy(dir) })
	}

	if n := reconcileGrokLogin(real); n != 1 {
		t.Fatalf("rewrote %d homes, want exactly the real home", n)
	}
	if got := grokKeyIn(t, real); got != "copy-new" {
		t.Errorf("real home holds %q, want the session's renewed credential", got)
	}
	if got := grokKeyIn(t, otherAccount); got != "other-newest" {
		t.Errorf("another account's copy was rewritten to %q", got)
	}
	if _, err := os.Stat(filepath.Join(real, "auth.json.aix-tmp")); !os.IsNotExist(err) {
		t.Error("the staging file must not be left behind")
	}
	// A second pass changes nothing: everything already holds the newest.
	if n := reconcileGrokLogin(real); n != 0 {
		t.Errorf("second reconcile rewrote %d homes, want none", n)
	}
}

// TestReconcileGrokLogin_FansARenewedRealHomeOutToEveryLiveCopy: after the
// real home renews, every same-account copy holds a refresh token xAI is
// about to revoke; each must receive the renewed file.
func TestReconcileGrokLogin_FansARenewedRealHomeOutToEveryLiveCopy(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-new", "dan@example.com", now, now.Add(6*time.Hour))

	copies := []string{t.TempDir(), t.TempDir()}
	for i, dir := range copies {
		writeGrokAuthMinted(t, dir, "copy-old", "dan@example.com", now.Add(-time.Duration(i+1)*time.Hour), now.Add(time.Hour))
		grokLogin.acquireCopy(dir)
		t.Cleanup(func() { grokLogin.releaseCopy(dir) })
	}

	if n := reconcileGrokLogin(real); n != 2 {
		t.Fatalf("rewrote %d homes, want both copies", n)
	}
	for _, dir := range copies {
		if got := grokKeyIn(t, dir); got != "real-new" {
			t.Errorf("copy %s holds %q, want the renewed credential", dir, got)
		}
	}
	if got := grokKeyIn(t, real); got != "real-new" {
		t.Errorf("real home changed to %q", got)
	}
}

// TestReconcileGrokLogin_OrdersByCreateTimeNotByWhoIsReal: the real home is
// not privileged — an older real credential loses to a newer copy, and vice
// versa, purely on the CLI's create_time.
func TestReconcileGrokLogin_OrdersByCreateTimeNotByWhoIsReal(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real", "dan@example.com", now.Add(-2*time.Hour), now.Add(4*time.Hour))
	copyHome := t.TempDir()
	// Newer expiry but OLDER mint: a clock skew must not make it win.
	writeGrokAuthMinted(t, copyHome, "copy", "dan@example.com", now.Add(-3*time.Hour), now.Add(5*time.Hour))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	reconcileGrokLogin(real)
	if got := grokKeyIn(t, real); got != "real" {
		t.Errorf("real home lost to an older credential: %q", got)
	}
	if got := grokKeyIn(t, copyHome); got != "real" {
		t.Errorf("copy holds %q, want the newer real credential", got)
	}
}

// TestGrokLoginKeeperOnce_RenewsAheadOfExpiryWhileCopiesAreLive: a real home
// inside the keep-ahead window renews even though a session copy is live,
// and the copy receives the renewed file in the same tick.
func TestGrokLoginKeeperOnce_RenewsAheadOfExpiryWhileCopiesAreLive(t *testing.T) {
	real := isolateGrok(t)
	resetGrokKeeper(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-5*time.Hour), now.Add(grokLoginKeepAhead-time.Minute))

	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "real-old", "dan@example.com", now.Add(-5*time.Hour), now.Add(grokLoginKeepAhead-time.Minute))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	renewals := 0
	runGrokLoginRenewal = func(_ context.Context, grokPath, base string) {
		renewals++
		if grokPath != "grok" || base != real {
			t.Errorf("renewal ran as %q on %q, want the real home", grokPath, base)
		}
		// What `grok models` does: rotates the credential in the real home.
		writeGrokAuthMinted(t, real, "real-renewed", "dan@example.com", now, now.Add(6*time.Hour))
	}

	if !grokLoginKeeperOnce(context.Background(), "grok", now) {
		t.Fatal("keeper did not renew a login inside the keep-ahead window")
	}
	if renewals != 1 {
		t.Fatalf("renewals=%d, want one", renewals)
	}
	if got := grokKeyIn(t, copyHome); got != "real-renewed" {
		t.Errorf("live copy holds %q after the tick, want the renewed credential", got)
	}

	// Renewed: the next tick has nothing to do.
	if grokLoginKeeperOnce(context.Background(), "grok", now.Add(grokLoginKeeperTick)) {
		t.Error("keeper renewed again with five hours left")
	}
	if renewals != 1 {
		t.Errorf("renewals=%d after a quiet tick, want still one", renewals)
	}
}

// TestGrokLoginKeeperOnce_LeavesAFreshLoginAndANonRefreshableOneAlone: nothing
// to do with hours left; and a login without a refresh token (an API-key or
// legacy file) is not Grok's to renew, so no `grok models` is spawned for it.
func TestGrokLoginKeeperOnce_LeavesAFreshLoginAndANonRefreshableOneAlone(t *testing.T) {
	real := isolateGrok(t)
	resetGrokKeeper(t)
	now := time.Now()
	runGrokLoginRenewal = func(context.Context, string, string) { t.Error("renewal must not run") }

	writeGrokAuthMinted(t, real, "fresh", "dan@example.com", now, now.Add(6*time.Hour))
	if grokLoginKeeperOnce(context.Background(), "grok", now) {
		t.Error("renewed a login with six hours left")
	}

	auth := map[string]any{grokExactOIDCScope: map[string]any{
		"key": "no-refresh", "email": "dan@example.com", "user_id": "u",
		"expires_at": now.Add(time.Minute).UTC().Format(time.RFC3339Nano),
	}}
	b, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(real, "auth.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if grokLoginKeeperOnce(context.Background(), "grok", now) {
		t.Error("tried to renew a credential that has no refresh token")
	}
}

// TestGrokLoginKeeperOnce_ARenewalThatChangedNothingIsNotRetriedEveryTick: a
// missing CLI or an unreachable xAI must not spawn `grok models` twenty
// times a minute for the rest of the token's life.
func TestGrokLoginKeeperOnce_ARenewalThatChangedNothingIsNotRetriedEveryTick(t *testing.T) {
	real := isolateGrok(t)
	resetGrokKeeper(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "stuck", "dan@example.com", now.Add(-6*time.Hour), now.Add(time.Minute))
	renewals := 0
	runGrokLoginRenewal = func(context.Context, string, string) { renewals++ }

	for i := 0; i < 5; i++ {
		grokLoginKeeperOnce(context.Background(), "grok", now.Add(time.Duration(i)*grokLoginKeeperTick))
	}
	if renewals != 1 {
		t.Fatalf("renewals=%d over five ticks, want one", renewals)
	}
	grokLoginKeeperOnce(context.Background(), "grok", now.Add(grokLoginRenewRetry))
	if renewals != 2 {
		t.Errorf("renewals=%d after the retry window, want two", renewals)
	}
}

// TestRunGrokLoginRenewal_RaisesTheCLIsEarlyInvalidationHorizon: `grok models`
// only refreshes within five minutes of expiry on its own; the renewal must
// tell the CLI to refresh now, on the real home, with no other GROK_* sink.
func TestRunGrokLoginRenewal_RaisesTheCLIsEarlyInvalidationHorizon(t *testing.T) {
	real := isolateGrok(t)
	t.Setenv("GROK_HOME", real)
	t.Setenv("XAI_API_KEY", "must-not-leak")
	prevRunner := cliAgentModelProbeRunner
	t.Cleanup(func() { cliAgentModelProbeRunner = prevRunner })
	var gotEnv []string
	var gotArgs []string
	cliAgentModelProbeRunner = func(_ context.Context, executable string, env []string, args ...string) (string, bool) {
		gotEnv, gotArgs = env, args
		return "", true
	}
	// isolateGrok replaced the renewal seam with a failing stub; run the real one.
	renewGrokLoginWithCLI(context.Background(), "grok", real)

	if strings.Join(gotArgs, " ") != "models" {
		t.Errorf("args=%q, want the models list", gotArgs)
	}
	want := map[string]string{
		"GROK_HOME":                  real,
		grokAuthEarlyInvalidationEnv: "1800",
	}
	for name, value := range want {
		if got := envValue(gotEnv, name); got != value {
			t.Errorf("%s=%q, want %q", name, got, value)
		}
	}
	if got := envValue(gotEnv, "XAI_API_KEY"); got != "" {
		t.Errorf("XAI_API_KEY leaked into the renewal child")
	}
}

func envValue(env []string, name string) string {
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// TestSweepStaleIsolatedGrokHomes_RemovesOnlyOldUnownedHomes: a home a live
// copy owns and a home younger than the age floor stay; an old orphan goes;
// unrelated temp entries are untouched.
func TestSweepStaleIsolatedGrokHomes_RemovesOnlyOldUnownedHomes(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{"TMP", "TEMP", "TMPDIR"} {
		t.Setenv(name, tmp)
	}
	if got := filepath.Clean(os.TempDir()); got != filepath.Clean(tmp) {
		t.Skipf("os.TempDir()=%s does not follow the env on this platform", got)
	}
	now := time.Now()
	mk := func(name string, age time.Duration) string {
		dir := filepath.Join(tmp, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(-age)
		if err := os.Chtimes(dir, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	old := mk(grokIsolatedHomePrefix+"old", 48*time.Hour)
	young := mk(grokIsolatedHomePrefix+"young", time.Hour)
	owned := mk(grokIsolatedHomePrefix+"owned", 48*time.Hour)
	unrelated := mk("something-else-", 48*time.Hour)
	grokLogin.acquireCopy(owned)
	t.Cleanup(func() { grokLogin.releaseCopy(owned) })

	if n := sweepStaleIsolatedGrokHomes(now); n != 1 {
		t.Fatalf("removed %d, want exactly the old orphan", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old orphan still present")
	}
	for _, keep := range []string{young, owned, unrelated} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
}

// TestReconcileGrokLogin_NeverRecreatesASignedOutRealHome: an absent real
// credential is `grok logout` (or the CLI signing the home out) and must stay
// absent — a live copy, even a fresh one of the same account, must not sign
// the persistent home back in; and copies never decide what the account is.
func TestReconcileGrokLogin_NeverRecreatesASignedOutRealHome(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	copies := []string{t.TempDir(), t.TempDir()}
	writeGrokAuthMinted(t, copies[0], "copy-dan", "dan@example.com", now, now.Add(6*time.Hour))
	writeGrokAuthMinted(t, copies[1], "copy-someone", "someone@example.com", now.Add(time.Minute), now.Add(6*time.Hour))
	for _, dir := range copies {
		grokLogin.acquireCopy(dir)
		t.Cleanup(func() { grokLogin.releaseCopy(dir) })
	}

	if n := reconcileGrokLogin(real); n != 0 {
		t.Fatalf("rewrote %d homes with no real credential, want none", n)
	}
	if _, err := os.Stat(filepath.Join(real, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("a signed-out real home was recreated from a live copy")
	}
	for i, dir := range copies {
		want := []string{"copy-dan", "copy-someone"}[i]
		if got := grokKeyIn(t, dir); got != want {
			t.Errorf("copy %d changed to %q, want %q untouched", i, got, want)
		}
	}
}

// TestGrokLoginKeeperOnce_WaitsOutAnotherRenewalRatherThanReconcilingBesideIt:
// the tick's renewal and reconciliation run under the login lock. While the
// live probe holds it, a tick neither renews nor reconciles — a snapshot taken
// beside a renewal in flight could write the older credential over the newer
// one — and it gives the lock up after a bounded wait rather than blocking.
func TestGrokLoginKeeperOnce_WaitsOutAnotherRenewalRatherThanReconcilingBesideIt(t *testing.T) {
	real := isolateGrok(t)
	resetGrokKeeper(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-6*time.Hour), now.Add(time.Minute))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-new", "dan@example.com", now, now.Add(6*time.Hour))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })
	runGrokLoginRenewal = func(context.Context, string, string) { t.Error("renewal must not run beside another") }

	release, ok := grokLogin.beginRenewal(context.Background())
	if !ok {
		t.Fatal("could not hold the login lock")
	}
	started := time.Now()
	if grokLoginKeeperOnce(context.Background(), "grok", now) {
		t.Fatal("tick renewed while another renewal held the login")
	}
	if waited := time.Since(started); waited < grokLoginReconcileWait || waited > grokLoginReconcileWait+3*time.Second {
		t.Errorf("tick waited %s, want about %s", waited, grokLoginReconcileWait)
	}
	if got := grokKeyIn(t, real); got != "real-old" {
		t.Errorf("real home rewritten to %q while the lock was held elsewhere", got)
	}
	release()

	// With the lock free the same tick reconciles the copy's newer credential
	// back (the stale real credential is not renewed: the renewal stub above
	// would fail the test, so the retry window is spent first).
	grokKeeper.mu.Lock()
	grokKeeper.lastRenewTry, grokKeeper.lastRenewFrom = now, now.Add(-6*time.Hour)
	grokKeeper.mu.Unlock()
	grokLoginKeeperOnce(context.Background(), "grok", now)
	if got := grokKeyIn(t, real); got != "copy-new" {
		t.Errorf("real home holds %q once the lock was free, want the copy's newer credential", got)
	}
}

// TestRemoveIsolatedGrokHome_WritesItsRenewalBackBeforeTheFileDies: a short
// isolated run (`grok models` discovery) that refreshed its copy is removed
// seconds later, long before the next keeper tick. The removal itself must
// hand the renewed credential to the real home, or it dies with the copy and
// the real home is left with the superseded refresh token.
func TestRemoveIsolatedGrokHome_WritesItsRenewalBackBeforeTheFileDies(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))

	home, err := setupIsolatedGrokSmokeHomeFrom(real)
	if err != nil {
		t.Fatal(err)
	}
	// What the CLI did inside the copy: rotated the credential.
	writeGrokAuthMinted(t, home, "copy-renewed", "dan@example.com", now, now.Add(6*time.Hour))

	if err := removeIsolatedGrokHome(home); err != nil {
		t.Fatal(err)
	}
	if got := grokKeyIn(t, real); got != "copy-renewed" {
		t.Errorf("real home holds %q after the copy was removed, want the copy's renewed credential", got)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Error("isolated home not removed")
	}
}

// TestGrokLoginKeeperOnce_ReconcilesACopysRenewalBeforeRenewingTheRealHome: the
// real home is inside the keep-ahead window but a live copy already holds a
// newer credential. Renewing the real home first would redeem a refresh token
// the copy superseded — and past the grace window the CLI would sign the real
// home out. The tick writes the copy back first and then has nothing to renew.
func TestGrokLoginKeeperOnce_ReconcilesACopysRenewalBeforeRenewingTheRealHome(t *testing.T) {
	real := isolateGrok(t)
	resetGrokKeeper(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-stale", "dan@example.com", now.Add(-6*time.Hour), now.Add(time.Minute))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-fresh", "dan@example.com", now.Add(-time.Minute), now.Add(6*time.Hour))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })
	runGrokLoginRenewal = func(context.Context, string, string) {
		t.Error("renewed the stale real home instead of taking the copy's newer credential")
	}

	if grokLoginKeeperOnce(context.Background(), "grok", now) {
		t.Fatal("tick renewed")
	}
	if got := grokKeyIn(t, real); got != "copy-fresh" {
		t.Errorf("real home holds %q, want the copy's fresher credential", got)
	}
}

// TestProbeGrokBillingLive_RenewsTheCopysNewerChainNotTheSupersededRealOne:
// every token is stale, but a live copy holds the NEWER credential (it
// refreshed on its own; that access token has since expired too). The real
// home's refresh token was superseded by that copy's, so renewing the real
// home as it stands would redeem a revoked token and sign the home out. The
// probe writes the copy back under the lock FIRST, then renews the real home
// — which now carries the copy's chain.
func TestProbeGrokBillingLive_RenewsTheCopysNewerChainNotTheSupersededRealOne(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, home, "real-superseded", "a@example.com", now.Add(-12*time.Hour), now.Add(-6*time.Hour))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-newer", "a@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	heldAtRenewal := ""
	runGrokLoginRenewal = func(_ context.Context, _, base string) {
		heldAtRenewal = grokKeyIn(t, base)
		writeGrokAuthMinted(t, base, "renewed", "a@example.com", now, now.Add(6*time.Hour))
	}
	calls := grokBillingServer(t, func(auth string) (int, string) {
		if auth != "Bearer renewed" {
			return 401, `{}`
		}
		return 200, grokFixtureBody(now.Add(72 * time.Hour))
	})

	if got := probeGrokBillingLive(context.Background(), "grok", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok after renewing the copy's chain", got)
	}
	if heldAtRenewal != "copy-newer" {
		t.Errorf("real home held %q when the renewal ran, want the copy's newer credential written back first", heldAtRenewal)
	}
	if *calls != 1 {
		t.Errorf("requests=%d, want one (the renewed token)", *calls)
	}
	if got := grokKeyIn(t, copyHome); got != "renewed" {
		t.Errorf("live copy holds %q, want the renewed credential fanned out", got)
	}
}

// TestProbeGrokBillingLive_401OnAFreshCopyStillRenewsThatCopyChain: the copy's
// access token is timestamp-fresh (so the first request uses it) but xAI
// returns 401. Reconciling that copy back is not enough — its refresh chain
// still has to be rotated — so the probe must run the CLI renewal against the
// copy's credential, not skip it just because expires_at is in the future.
func TestProbeGrokBillingLive_401OnAFreshCopyStillRenewsThatCopyChain(t *testing.T) {
	home := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, home, "real-superseded", "a@example.com", now.Add(-6*time.Hour), now.Add(5*time.Hour))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-newer", "a@example.com", now.Add(-time.Minute), now.Add(6*time.Hour))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	heldAtRenewal := ""
	runGrokLoginRenewal = func(_ context.Context, _, base string) {
		heldAtRenewal = grokKeyIn(t, base)
		writeGrokAuthMinted(t, base, "renewed", "a@example.com", now, now.Add(6*time.Hour))
	}
	calls := grokBillingServer(t, func(auth string) (int, string) {
		if auth != "Bearer renewed" {
			return 401, `{}`
		}
		return 200, grokFixtureBody(now.Add(72 * time.Hour))
	})

	if got := probeGrokBillingLive(context.Background(), "grok", time.Now); got != grokLiveOutcomeOK {
		t.Fatalf("outcome=%q, want ok after rotating the copy's 401'd chain", got)
	}
	if heldAtRenewal != "copy-newer" {
		t.Errorf("real home held %q when the renewal ran, want the copy's newer credential", heldAtRenewal)
	}
	if *calls < 2 {
		t.Errorf("requests=%d, want the 401 then the renewed token", *calls)
	}
}

// TestRemoveIsolatedGrokHome_KeepsTheCopyUntilItsCredentialIsWrittenBack: a
// removal that cannot reconcile because a renewal holds the login for longer
// than it waits must NOT delete the copy — files or registration — and must
// retry once the lock is free, writing the copy's credential back then.
func TestRemoveIsolatedGrokHome_KeepsTheCopyUntilItsCredentialIsWrittenBack(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))
	home, err := setupIsolatedGrokSmokeHomeFrom(real)
	if err != nil {
		t.Fatal(err)
	}
	writeGrokAuthMinted(t, home, "copy-renewed", "dan@example.com", now, now.Add(6*time.Hour))

	prevWait := grokLoginRemovalReconcileWait
	grokLoginRemovalReconcileWait = 100 * time.Millisecond
	t.Cleanup(func() { grokLoginRemovalReconcileWait = prevWait })

	release, ok := grokLogin.beginRenewal(context.Background())
	if !ok {
		t.Fatal("could not hold the login lock")
	}
	if err := removeIsolatedGrokHome(home); err == nil || !strings.Contains(err.Error(), "renewal in flight") {
		t.Fatalf("removal err=%v, want the login-busy deferral", err)
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal("the copy was deleted while its credential was still the only live one")
	}
	if got := grokKeyIn(t, real); got != "real-old" {
		t.Fatalf("real home rewritten to %q while the lock was held elsewhere", got)
	}
	registered := false
	for _, live := range grokLogin.liveCopies() {
		if live == filepath.Clean(home) {
			registered = true
		}
	}
	if !registered {
		t.Fatal("the deferred copy was unregistered")
	}
	release()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(home); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("the retry never removed the copy once the lock was free")
	}
	if got := grokKeyIn(t, real); got != "copy-renewed" {
		t.Errorf("real home holds %q, want the copy's credential written back by the retry", got)
	}
}

// TestRemoveIsolatedGrokHome_NeverDeletesWhileTheLoginLockStaysHeld: even after
// every retry has fired, a copy whose credential was never written back must
// still be on disk and still registered — a stuck renewal is not a reason to
// throw away the only live refresh token.
func TestRemoveIsolatedGrokHome_NeverDeletesWhileTheLoginLockStaysHeld(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))
	home, err := setupIsolatedGrokSmokeHomeFrom(real)
	if err != nil {
		t.Fatal(err)
	}
	writeGrokAuthMinted(t, home, "copy-renewed", "dan@example.com", now, now.Add(6*time.Hour))

	prevWait := grokLoginRemovalReconcileWait
	grokLoginRemovalReconcileWait = 20 * time.Millisecond
	t.Cleanup(func() { grokLoginRemovalReconcileWait = prevWait })

	release, ok := grokLogin.beginRenewal(context.Background())
	if !ok {
		t.Fatal("could not hold the login lock")
	}
	t.Cleanup(release)

	if err := removeIsolatedGrokHome(home); err == nil || !errors.Is(err, errGrokLoginBusy) {
		t.Fatalf("removal err=%v, want errGrokLoginBusy", err)
	}
	time.Sleep(grokLoginRemovalReconcileWait*time.Duration(grokLoginRemovalRetries+2) + 200*time.Millisecond)
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal("the copy was deleted after retries while the lock was still held")
	}
	if got := grokKeyIn(t, real); got != "real-old" {
		t.Errorf("real home rewritten to %q while the lock was held elsewhere", got)
	}
	registered := false
	for _, live := range grokLogin.liveCopies() {
		if live == filepath.Clean(home) {
			registered = true
		}
	}
	if !registered {
		t.Fatal("the copy was unregistered while still the only live credential")
	}
}

// TestReconcileGrokLogin_DoesNotOverwriteACopyThatRefreshedUnderTheSnapshot:
// isolated Grok children never take the login lock, so a copy can refresh
// itself between the snapshot and the write. A destination that now holds a
// credential at least as new as the chosen one must be left alone — writing
// the snapshot over it would put the account on the chain that refresh just
// superseded.
func TestReconcileGrokLogin_DoesNotOverwriteACopyThatRefreshedUnderTheSnapshot(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-newest", "dan@example.com", now.Add(-time.Minute), now.Add(6*time.Hour))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-old", "dan@example.com", now.Add(-3*time.Hour), now.Add(3*time.Hour))
	grokLogin.acquireCopy(copyHome)
	t.Cleanup(func() { grokLogin.releaseCopy(copyHome) })

	// The snapshot reconcileGrokLogin would take, then the child refreshes.
	newest, ok := readGrokCredentialStamp(real)
	if !ok {
		t.Fatal("real stamp unreadable")
	}
	writeGrokAuthMinted(t, copyHome, "copy-refreshed-under-us", "dan@example.com", now, now.Add(6*time.Hour))
	if err := replaceGrokAuthFileIfOlder(copyHome, newest); err == nil {
		t.Fatal("a copy that refreshed itself since the snapshot was overwritten")
	}
	if got := grokKeyIn(t, copyHome); got != "copy-refreshed-under-us" {
		t.Errorf("copy holds %q, want its own newer refresh kept", got)
	}
	// And the next pass carries that newer credential the other way.
	reconcileGrokLogin(real)
	if got := grokKeyIn(t, real); got != "copy-refreshed-under-us" {
		t.Errorf("real home holds %q, want the copy's newer refresh on the next pass", got)
	}
}

// TestReplaceGrokAuthFile_WaitsOutTheCLIsOwnLock: the CLI takes
// auth.json.lock around its credential writes; a replacement must take the
// same lock, so it never lands between a child's re-check and its write. With
// the lock held elsewhere for the whole wait, nothing is written and no
// staging file is left; once it is free, the same replacement lands.
func TestReplaceGrokAuthFile_WaitsOutTheCLIsOwnLock(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-new", "dan@example.com", now, now.Add(6*time.Hour))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-old", "dan@example.com", now.Add(-time.Hour), now.Add(5*time.Hour))
	newest, ok := readGrokCredentialStamp(real)
	if !ok {
		t.Fatal("real stamp unreadable")
	}

	held, err := os.OpenFile(filepath.Join(copyHome, grokAuthLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockFileExclusive(held); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = replaceGrokAuthFileIfOlder(copyHome, newest)
	if err == nil || !strings.Contains(err.Error(), "held by the CLI") {
		t.Fatalf("replacement err=%v, want the CLI-lock refusal", err)
	}
	if waited := time.Since(started); waited < grokAuthLockWait || waited > grokAuthLockWait+2*time.Second {
		t.Errorf("waited %s, want about %s", waited, grokAuthLockWait)
	}
	if got := grokKeyIn(t, copyHome); got != "copy-old" {
		t.Errorf("copy rewritten to %q while the CLI held its lock", got)
	}
	if _, err := os.Stat(filepath.Join(copyHome, "auth.json.aix-tmp")); !os.IsNotExist(err) {
		t.Error("staging file left behind")
	}
	_ = unlockFile(held)
	_ = held.Close()

	if err := replaceGrokAuthFileIfOlder(copyHome, newest); err != nil {
		t.Fatalf("replacement after the lock was freed: %v", err)
	}
	if got := grokKeyIn(t, copyHome); got != "real-new" {
		t.Errorf("copy holds %q, want the replacement once the lock was free", got)
	}
}

// TestSetupIsolatedGrokHome_SeedsTheCopyUnderTheLoginLock: creating a copy
// waits for a renewal in flight — registration and the credential copy are
// one step under the login lock — so the child never starts on a credential
// the renewal is about to supersede, and no renewal can reconcile past a
// registered-but-empty home.
func TestSetupIsolatedGrokHome_SeedsTheCopyUnderTheLoginLock(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "before-renewal", "dan@example.com", now.Add(-time.Hour), now.Add(5*time.Hour))

	release, ok := grokLogin.beginRenewal(context.Background())
	if !ok {
		t.Fatal("could not hold the login lock")
	}
	type result struct {
		home string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		home, err := setupIsolatedGrokSmokeHomeFrom(real)
		done <- result{home, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("copy seeded while a renewal held the login (home=%q err=%v)", r.home, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	// What the renewal does before it releases: rotates the real credential.
	writeGrokAuthMinted(t, real, "after-renewal", "dan@example.com", now, now.Add(6*time.Hour))
	release()

	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("copy never seeded after the renewal released the login")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { _ = removeIsolatedGrokHome(r.home) })
	if got := grokKeyIn(t, r.home); got != "after-renewal" {
		t.Errorf("copy holds %q, want the credential as renewed", got)
	}
}

// TestSetupIsolatedGrokHome_GivesUpOnAStuckRenewal: a lock held past the seed
// wait is a stuck renewal; the home is not created beside it.
func TestSetupIsolatedGrokHome_GivesUpOnAStuckRenewal(t *testing.T) {
	real := isolateGrok(t)
	writeGrokAuth(t, real, "k", "dan@example.com", time.Now().Add(time.Hour))
	prev := grokLoginSeedWait
	grokLoginSeedWait = 150 * time.Millisecond
	t.Cleanup(func() { grokLoginSeedWait = prev })
	release, ok := grokLogin.beginRenewal(context.Background())
	if !ok {
		t.Fatal("could not hold the login lock")
	}
	defer release()
	registeredBefore := len(grokLogin.liveCopies())
	home, err := setupIsolatedGrokSmokeHomeFrom(real)
	if err == nil {
		_ = removeIsolatedGrokHome(home)
		t.Fatal("isolated home created beside a renewal that never released")
	}
	if got := len(grokLogin.liveCopies()); got != registeredBefore {
		t.Errorf("registered copies %d -> %d: a home that was never created stayed registered", registeredBefore, got)
	}
}

// TestReconcileGrokLogin_DoesNotRecreateARealHomeSignedOutUnderTheSnapshot:
// the snapshot chose a live copy as newest; then, before the write, the real
// credential vanished (`grok logout`). The under-lock check must abort, not
// treat the missing file as writable.
func TestReconcileGrokLogin_DoesNotRecreateARealHomeSignedOutUnderTheSnapshot(t *testing.T) {
	real := isolateGrok(t)
	now := time.Now()
	writeGrokAuthMinted(t, real, "real-old", "dan@example.com", now.Add(-6*time.Hour), now.Add(-time.Minute))
	copyHome := t.TempDir()
	writeGrokAuthMinted(t, copyHome, "copy-new", "dan@example.com", now, now.Add(6*time.Hour))
	newest, ok := readGrokCredentialStamp(copyHome)
	if !ok {
		t.Fatal("copy stamp unreadable")
	}
	if err := os.Remove(filepath.Join(real, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if err := replaceGrokAuthFileIfOlder(real, newest); err == nil {
		t.Fatal("a real home signed out under the snapshot was recreated")
	}
	if _, err := os.Stat(filepath.Join(real, "auth.json")); !os.IsNotExist(err) {
		t.Error("auth.json recreated after logout")
	}
	if _, err := os.Stat(filepath.Join(real, "auth.json.aix-tmp")); !os.IsNotExist(err) {
		t.Error("staging file left behind")
	}
}
