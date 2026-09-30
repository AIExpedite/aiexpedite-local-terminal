// cliagent_smoke_opencode_redaction_test.go — nothing the CLI authored escapes
// the classifier.
//
// The published `__cli_smoke_result__` and the device log are both reachable by
// someone other than the person at the keyboard: the result travels to the cloud
// and the agent log rotates to disk and is uploaded with diagnostics. OpenCode's
// stdout/stderr are arbitrary vendor text that can carry a config fragment, a
// private path or a credential shape no denylist anticipates — and the prompt
// carries a marker nonce whose whole value is that it is unguessable. So the
// only sound rule is to keep no vendor-authored bytes at all, and these cases
// prove it BY CONSTRUCTION for every failure arm.
package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// openCodeSecretStdout / …Stderr are payloads of exactly the shape a broken
// install produces, each carrying a distinct secret.
const (
	openCodeSecretToken  = "sk-live-DO-NOT-LEAK-0987654321"
	openCodeSecretPath   = "/Users/someone/private/opencode.json"
	openCodeSecretStderr = "error: unknown option '--pure'\n  provider key " +
		openCodeSecretToken + " loaded from " + openCodeSecretPath
)

func TestRunOpenCodeSmoke_ResultAndLogCarryNoVendorText(t *testing.T) {
	arms := []struct {
		name           string
		stdout, stderr string
		runErr         func(*testing.T) error
	}{
		{
			name:   "flag rejection",
			stderr: openCodeSecretStderr,
			runErr: openCodeExitError,
		},
		{
			name:   "generic non-zero exit",
			stdout: `{"type":"text","text":"` + openCodeSecretToken + `"}`,
			stderr: "panic at " + openCodeSecretPath,
			runErr: openCodeExitError,
		},
		{
			name:   "clean exit with no envelope",
			stdout: `{"type":"text","text":"` + openCodeSecretPath + `"}`,
		},
		{
			// no_output: not one frame, only vendor prose on both streams.
			name:   "no output",
			stdout: "opencode: updating from " + openCodeSecretPath + "\n",
			stderr: "key " + openCodeSecretToken,
		},
		{
			name:   "auth error event",
			stdout: `{"type":"error","error":{"message":"authentication failed for ` + openCodeSecretToken + `"}}`,
		},
		{
			name:   "provider error event",
			stdout: `{"type":"error","error":{"message":"usage limit reached; key ` + openCodeSecretToken + `"}}`,
		},
		{
			name:   "marker mismatch",
			stdout: `{"type":"text","text":"I cannot, see ` + openCodeSecretPath + `"}` + "\n" + `{"type":"session.completed"}`,
		},
		{
			name:   "launch error",
			runErr: func(*testing.T) error { return errOpenCodeShimUnrenderable },
		},
	}

	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			openCodeSmokeEnv(t)
			path := stubOpenCodeBinary(t)
			stubOpenCodeReadiness(t, "", false)
			var marker, promptFile string
			stubOpenCodeSmokeExec(t, func(_ context.Context, launch openCodeLaunch) ([]byte, []byte, error) {
				marker = openCodeMarkerFromLaunch(t, launch)
				promptFile = launch.PromptFile
				var err error
				if arm.runErr != nil {
					err = arm.runErr(t)
				}
				return []byte(arm.stdout), []byte(arm.stderr), err
			})

			var result cliSmokeResult
			logged := captureStdout(t, func() {
				result = runOpenCodeSmoke(context.Background(), path, "0.9.1")
			})

			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			forbidden := map[string]string{
				"the marker nonce":  marker,
				"a stderr fragment": openCodeSecretToken,
				"a private path":    openCodeSecretPath,
				"the prompt prefix": openCodeMaintenanceSmokePromptPrefix,
				"the resolved path": path,
				"the prompt file":   promptFile,
			}
			for what, secret := range forbidden {
				if secret == "" {
					continue
				}
				if strings.Contains(string(encoded), secret) {
					t.Errorf("published result carries %s: %s", what, encoded)
				}
				if strings.Contains(logged, secret) {
					t.Errorf("device log carries %s: %s", what, logged)
				}
			}
			// An argv token is a fixed flag, but assert the ladder's tokens are
			// not reflected either — the shape ID is what names the rung.
			if strings.Contains(string(encoded), "--format") {
				t.Errorf("published result reflects argv: %s", encoded)
			}
			if result.Diagnostic == "" || result.Diagnostic == cliSmokeDiagnosticNone {
				t.Errorf("arm produced no diagnostic: %+v", result)
			}
		})
	}
}

func TestOpenCodeSmokeFailureLogLine_CarriesOnlyClosedValues(t *testing.T) {
	// The signature takes COUNTS, not bytes: a function that cannot receive
	// vendor text cannot leak it, however a future caller wires it up.
	line := openCodeSmokeFailureLogLine(
		openCodeRunShapeIDPlain, cliUsageErrorProtocol, cliSmokeDiagnosticNoOutput,
		openCodeSmokeCounts{StderrBytes: 4096, StdoutBytes: 128, Frames: 3, EscLines: 2, Terminal: false, Exit: 1})
	for _, want := range []string{
		"opencode", openCodeRunShapeIDPlain,
		"category=" + cliUsageErrorProtocol,
		"diagnostic=" + cliSmokeDiagnosticNoOutput,
		"stderrBytes=4096", "stdoutBytes=128",
		"frames=3", "escLines=2", "terminal=false", "exit=1",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q is missing %q", line, want)
		}
	}
	if strings.ContainsAny(strings.TrimSpace(stripANSIForTest(line)), "\n") {
		t.Errorf("log line spans several lines: %q", line)
	}
}

// stripANSIForTest removes the colour codes the log helpers wrap their lines in,
// so an assertion reads the text rather than the terminal escape.
func stripANSIForTest(s string) string {
	return ansiEscapeRe.ReplaceAllString(s, "")
}
