package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// codexAPIKeyLogoutFixture is a Codex home whose persisted OAuth login is gone
// (`codex login status` answers a definite logout) but whose leftover
// auth.json still carries an id_token expiry — the state in which an API key is
// what actually authenticates.
func codexAPIKeyLogoutFixture(t *testing.T, envKey, fileKey string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("OPENAI_API_KEY", envKey)
	t.Setenv("AIEXPEDITE_CODEX_RL_CACHE", filepath.Join(t.TempDir(), "rl.json"))
	auth := map[string]any{
		"email":  "dev@example.com",
		"tokens": map[string]any{"id_token": helperJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()})},
	}
	if fileKey != "" {
		auth["OPENAI_API_KEY"] = fileKey
	}
	helperWriteJSON(t, filepath.Join(home, "auth.json"), auth)
	stubCodexLogin(t, false, true)
	return home
}

// The carve-out lives in the smoke's login check ONLY. codexProbeLoginStatus
// keeps reporting the raw definite logout, because ParseContext acts on it:
// the leftover id_token expiry describes a credential Codex will not use, so
// it is cleared rather than shown.
func TestCodexProbeLoginStatus_KeepsTheRawLogoutWithAnAPIKey(t *testing.T) {
	home := codexAPIKeyLogoutFixture(t, "sk-env-key", "")

	if loggedIn, known := codexProbeLoginStatus(context.Background(), "codex"); loggedIn || !known {
		t.Fatalf("codexProbeLoginStatus = (%v, %v), want the raw definite logout", loggedIn, known)
	}
	usage, _ := codexUsageParser{}.Parse(home, detectedCLIAgent{Detected: true, Path: "codex"}, time.Now())
	if usage.Authenticated == nil || !*usage.Authenticated || usage.AuthState != "authenticated" {
		t.Fatalf("API-key auth must stay authenticated: (%v, %q)", usage.Authenticated, usage.AuthState)
	}
	if usage.LoginExpiresAt != "" || usage.LoginExpirationState != loginExpirationNotReported {
		t.Fatalf("a definite OAuth logout must clear the leftover expiry: (%q, %q)", usage.LoginExpiresAt, usage.LoginExpirationState)
	}
}

// The smoke's check reports that same logout as INCONCLUSIVE when an API key —
// inherited or persisted — will authenticate the turn, so the cooldown replay
// keeps an API-key user's verdict instead of re-spending a turn on every call.
// Without a key the logout is reported unchanged.
func TestCodexSmokeLoginCheck_DowngradesALogoutOnlyWithAnAPIKey(t *testing.T) {
	for _, tc := range []struct {
		name            string
		envKey, fileKey string
		wantKnown       bool
	}{
		{name: "env key", envKey: "sk-env-key"},
		{name: "auth.json key", fileKey: "sk-file-key"},
		{name: "no key", wantKnown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexAPIKeyLogoutFixture(t, tc.envKey, tc.fileKey)
			loggedIn, known := codexSmokeLoginCheck(context.Background(), "codex")
			if loggedIn || known != tc.wantKnown {
				t.Fatalf("codexSmokeLoginCheck = (%v, %v), want (false, %v)", loggedIn, known, tc.wantKnown)
			}
		})
	}
}

// The downgrade is what keeps the cooldown honest for an API-key user: the
// replay re-runs the provider row's login check, and a definite logout there
// would drop the cached verdict and spend another turn.
func TestRunCLISmoke_CodexAPIKeyUserReplaysTheCooldown(t *testing.T) {
	codexSmokeEnv(t)
	codexAPIKeyLogoutFixture(t, "sk-env-key", "")
	path := stubCodexBinary(t)
	stubCodexSmokePath(t, path)
	calls, _ := stubCodexSmokeExec(t, func(ctx context.Context, launch codexSmokeLaunch) ([]byte, []byte, error) {
		return codexPreUpdateFrames(codexMarkerFromLaunch(t, launch)), nil, nil
	})

	for i := 0; i < 3; i++ {
		if result, _ := runCLISmoke(context.Background(), "codex"); result.Status != cliSmokeStatusSuccess {
			t.Fatalf("smoke %d = %+v, want success", i, result)
		}
	}
	if *calls != 1 {
		t.Fatalf("an API-key user spent %d turns across 3 smokes, want 1", *calls)
	}
}
