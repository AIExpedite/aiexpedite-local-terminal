// opencode_argv_test.go — the ONE OpenCode invocation contract, pinned.
//
// Two things used to be able to drift apart and did: the direct chat path and
// the terminal session path each shaped the same CLI with their own builder, and
// the signed legacy `session_start` smoke ran whatever argv the publisher signed.
// These cases pin the merged contract:
//
//   - `run --format json` is forced on every rung; `--session` is the one
//     droppable token; the prompt is NEVER on argv.
//   - diagnostic invocations pass through verbatim (reshaping one into a `run`
//     would spend a turn nobody asked for).
//   - recognition of the legacy wire envelope is BROAD (so a mutation fails
//     closed in StartSession instead of quietly running as an ordinary session)
//     but BOUNDED by the reserved token vocabulary, and validation is EXACT.
//   - the marker is OPAQUE on the legacy transport: no grammar is a
//     precondition, because the publisher's grammar is not visible from here.
package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// openCodeSmokePromptFor renders the reserved marker sentence for an arbitrary
// marker. Built from the constant, never a transcribed copy of it.
func openCodeSmokePromptFor(marker string) string {
	return openCodeMaintenanceSmokePromptPrefix + marker
}

/* --------------------------------------------------------------------------
   Invocation contract
   -------------------------------------------------------------------------- */

func TestBuildOpenCodeRunArgs_ForcedTokensOnEveryRung(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shape    openCodeRunShape
		nativeID string
		want     []string
	}{
		{"no-resume rung", openCodeRunShapeNoSession, "", []string{"run", "--format", "json"}},
		{
			"no-resume rung ignores a native id it was not given the flag for",
			openCodeRunShapeNoSession, "ses_abc", []string{"run", "--format", "json"},
		},
		{
			"resume rung adds --session adjacent to its id",
			openCodeRunShapeResume, "ses_abc", []string{"run", "--format", "json", "--session", "ses_abc"},
		},
		{
			"resume rung without an id degrades to the no-resume argv",
			openCodeRunShapeResume, "", []string{"run", "--format", "json"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildOpenCodeRunArgs(tc.shape, tc.nativeID)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			// --continue / --fork resume whatever the user last ran globally,
			// including in their own local TUI. Never.
			for _, a := range got {
				switch a {
				case "--continue", "-c", "--fork", "--pure", "--print-logs":
					t.Fatalf("argv must never carry %q: %q", a, got)
				}
			}
		})
	}
}

func TestOpenCodeShapeForResume_PicksTheLadderRung(t *testing.T) {
	if got := openCodeShapeForResume("ses_1"); got.ID != openCodeRunShapeIDSession {
		t.Errorf("a turn with a native id must prefer the resume rung, got %q", got.ID)
	}
	if got := openCodeShapeForResume(""); got.ID != openCodeRunShapeIDPlain {
		t.Errorf("a turn with no native id must use the no-resume rung, got %q", got.ID)
	}
}

func TestIsOpenCodeSynthesizedRun_AcceptsThePromptlessShape(t *testing.T) {
	// The prompt moved to stdin, so the gate's argv is promptless — a predicate
	// that required a trailing prompt token would stop auto-approving signed
	// session starts and hang every headless run at the approval dialog.
	if !isOpenCodeSynthesizedRun(buildOpenCodeRunArgs(openCodeRunShapeNoSession, "")) {
		t.Error("the promptless forced shape must be recognised as synthesized")
	}
	if !isOpenCodeSynthesizedRun(buildOpenCodeRunArgs(openCodeRunShapeResume, "ses_1")) {
		t.Error("the resume rung must be recognised as synthesized")
	}
	for _, args := range [][]string{
		nil,
		{"run"},
		{"run", "--format"},
		{"serve"},
		{"run", "--pure", "--format", "json"},
	} {
		if isOpenCodeSynthesizedRun(args) {
			t.Errorf("%q must not be recognised as the synthesized shape", args)
		}
	}
}

func TestIsOpenCodeDiagnosticInvocation_NeverBecomesAModelRun(t *testing.T) {
	for _, args := range [][]string{
		{"--version"}, {"-v"}, {"--help"}, {"-h"},
		{"models"}, {"auth", "list"}, {"serve"}, {"upgrade"},
		{"run", "--help"},
	} {
		if !isOpenCodeDiagnosticInvocation(args) {
			t.Errorf("%q must be treated as a diagnostic", args)
		}
	}
	for _, args := range [][]string{
		{"do the thing"},
		{"--model", "anthropic/x", "do the thing"},
		// A prompt that merely mentions a subcommand is not a diagnostic: the
		// subcommand only counts as the FIRST token.
		{"tell me about models"},
	} {
		if isOpenCodeDiagnosticInvocation(args) {
			t.Errorf("%q must not be treated as a diagnostic", args)
		}
	}
}

/* --------------------------------------------------------------------------
   Legacy wire contract — recognition
   -------------------------------------------------------------------------- */

// openCodeMarkerGrammars are markers of DIFFERENT shapes. None of them may be a
// precondition: the publisher's grammar is not visible from this repo, and
// Grok's own two transports already disagree about theirs.
var openCodeMarkerGrammars = map[string]string{
	"the probe's own grammar":   openCodeSmokeMarkerPrefix + "0a1b2c3d",
	"a Grok-style uppercase id": "AIEXPEDITE_OPENCODE_SMOKE_MARKER_AB12CD",
	"an opaque vendor token":    "zz-Marker.42/xyz",
}

// The reserved vocabulary is DERIVED from the frozen wire shapes, never
// re-listed. A hand-maintained copy would go stale the moment a third shape is
// frozen (or a token leaves one), and a stale vocabulary stops recognising the
// deployed envelope — letting its tokens reach the ordinary session path, which
// is the exact bug this feature fixes.
func TestOpenCodeSmokeReservedTokens_AreDerivedFromTheFrozenWireShapes(t *testing.T) {
	for _, wire := range openCodeSmokeWireRequests {
		for _, token := range wire {
			if !openCodeSmokeReservedTokens[token] {
				t.Errorf("token %q of frozen wire %q is not in the reserved vocabulary", token, wire)
			}
		}
	}
	// And nothing beyond them: a wider vocabulary would start promoting ordinary
	// caller traffic, where a malformed combination fails closed.
	inWire := map[string]bool{}
	for _, wire := range openCodeSmokeWireRequests {
		for _, token := range wire {
			inWire[token] = true
		}
	}
	for token := range openCodeSmokeReservedTokens {
		if !inWire[token] {
			t.Errorf("reserved vocabulary carries %q, which no frozen wire shape contains", token)
		}
	}
	// A shape added later is picked up with no second edit.
	extended := append(append([][]string{}, openCodeSmokeWireRequests...),
		[]string{"run", "--strict", "--format", "json"})
	prev := openCodeSmokeWireRequests
	openCodeSmokeWireRequests = extended
	t.Cleanup(func() { openCodeSmokeWireRequests = prev })
	if !computeOpenCodeSmokeReservedTokens()["--strict"] {
		t.Error("a newly frozen shape's token was not derived into the vocabulary")
	}
}

func TestOpenCodeMaintenanceSmokeRequest_RecognisesBothFrozenShapesAndMutations(t *testing.T) {
	for grammar, marker := range openCodeMarkerGrammars {
		prompt := openCodeSmokePromptFor(marker)
		for _, wire := range openCodeSmokeWireRequests {
			args := append(append([]string{}, wire...), prompt)
			if !openCodeMaintenanceSmokeRequest(args) {
				t.Errorf("%s: frozen wire %q was not recognised", grammar, args)
			}
		}
		// Mutations inside the reserved vocabulary are promoted TOO, so they
		// fail closed under validation rather than running as an ordinary
		// session with `--pure` on the child's command line.
		for _, mutation := range [][]string{
			{"run", "run", "--format", "json"},
			{"--format", "json", "run"},
			{"run", "--format"},
			{"run", "--pure", "json", "--format"},
			{"run"},
		} {
			args := append(append([]string{}, mutation...), prompt)
			if !openCodeMaintenanceSmokeRequest(args) {
				t.Errorf("%s: reserved-vocabulary mutation %q must be promoted", grammar, args)
			}
		}
	}
}

func TestOpenCodeMaintenanceSmokeRequest_NonReservedTrafficStaysAnOrdinarySession(t *testing.T) {
	marker := openCodeSmokeMarkerPrefix + "deadbeef"
	prompt := openCodeSmokePromptFor(marker)
	for _, args := range [][]string{
		// A non-reserved token means this is not maintenance traffic — so a
		// caller flag can NEVER turn into a refusal.
		append([]string{"run", "--format", "json", "--model", "x"}, prompt),
		append([]string{"run", "--format", "json", "--agent", "y"}, prompt),
		append([]string{"serve"}, prompt),
		// The marker prompt must be the LAST token.
		{"run", "--format", "json", prompt, "--model"},
		// No prompt at all, an empty marker, and a CR/LF marker.
		{"run", "--pure", "--format", "json"},
		{"run", "--format", "json", openCodeMaintenanceSmokePromptPrefix},
		{"run", "--format", "json", openCodeMaintenanceSmokePromptPrefix + "  "},
		{"run", "--format", "json", openCodeMaintenanceSmokePromptPrefix + "a\nb"},
		{"run", "--format", "json", openCodeMaintenanceSmokePromptPrefix + "a\rb"},
		// An ordinary prompt-only start.
		{"fix the failing test"},
		nil,
	} {
		if openCodeMaintenanceSmokeRequest(args) {
			t.Errorf("%q must stay an ordinary session", args)
		}
	}
}

/* --------------------------------------------------------------------------
   Legacy wire contract — validation
   -------------------------------------------------------------------------- */

func TestValidateOpenCodeSmokeRequest_AcceptsOnlyTheFrozenShapes(t *testing.T) {
	for grammar, marker := range openCodeMarkerGrammars {
		prompt := openCodeSmokePromptFor(marker)
		for _, wire := range openCodeSmokeWireRequests {
			args := append(append([]string{}, wire...), prompt)
			got, err := validateOpenCodeSmokeRequest(args)
			if err != nil {
				t.Fatalf("%s: frozen wire %q rejected: %v", grammar, args, err)
			}
			if got != prompt {
				t.Fatalf("%s: returned prompt %q, want %q", grammar, got, prompt)
			}
		}
	}
}

func TestValidateOpenCodeSmokeRequest_FailsClosedWithoutEchoingATokenOrSpawning(t *testing.T) {
	marker := openCodeSmokeMarkerPrefix + "0a1b2c3d"
	prompt := openCodeSmokePromptFor(marker)
	secret := "sk-live-DO-NOT-ECHO-12345"
	for _, args := range [][]string{
		// Reordered, duplicated, one token missing.
		{"--format", "json", "run", prompt},
		{"run", "run", "--format", "json", prompt},
		{"run", "--format", prompt},
		{"run", "--pure", "json", "--format", prompt},
		// Promoted but promptless / invalid marker.
		{"run", "--format", "json"},
		{"run", "--format", "json", "not the reserved sentence"},
		{"run", "--format", "json", openCodeMaintenanceSmokePromptPrefix},
		// A token carrying something secret must not be reflected back.
		{"run", "--format", "json", "--model", secret, prompt},
	} {
		got, err := validateOpenCodeSmokeRequest(args)
		if err == nil {
			t.Fatalf("%q was accepted; it must fail closed (returned %q)", args, got)
		}
		if err.Error() != errOpenCodeSmokeRequestContract.Error() {
			t.Fatalf("%q produced a non-fixed error %q", args, err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), marker) {
			t.Fatalf("error echoed a caller token: %q", err)
		}
	}
}

func TestValidateOpenCodeSmokeRequest_TheDeployedPureShapeNeverReachesAChild(t *testing.T) {
	// `--pure` stays in the frozen WIRE shape so the deployed request is
	// recognised — and is consumed by validation, because the child argv comes
	// from the ladder. The same split that keeps Grok's empty `--tools` operand
	// off its child's command line.
	prompt := openCodeSmokePromptFor(openCodeSmokeMarkerPrefix + "0a1b2c3d")
	wire := append(append([]string{}, openCodeSmokeWireRequests[0]...), prompt)
	if !openCodeArgvContains(wire, "--pure") {
		t.Fatalf("the first frozen wire shape must be the deployed --pure one: %q", wire)
	}
	if _, err := validateOpenCodeSmokeRequest(wire); err != nil {
		t.Fatalf("the deployed wire shape must be accepted: %v", err)
	}
	child := buildOpenCodeRunArgs(openCodeRunShapeNoSession, "")
	for _, a := range child {
		if a == "--pure" || a == prompt {
			t.Fatalf("child argv %q must carry neither --pure nor the prompt", child)
		}
	}
}

/* --------------------------------------------------------------------------
   Control token
   -------------------------------------------------------------------------- */

func TestExtractOpenCodeMaintenanceSmokeControl_ConsumesTheTokenEverywhere(t *testing.T) {
	prompt := openCodeSmokePromptFor(openCodeSmokeMarkerPrefix + "0a1b2c3d")
	for _, args := range [][]string{
		{openCodeMaintenanceSmokeControlArg, "run", "--format", "json", prompt},
		{"run", openCodeMaintenanceSmokeControlArg, "--format", "json", prompt},
		{"run", "--format", "json", prompt, openCodeMaintenanceSmokeControlArg},
	} {
		cleaned, present := extractOpenCodeMaintenanceSmokeControl(args)
		if !present {
			t.Fatalf("%q: control token not reported", args)
		}
		if openCodeArgvContains(cleaned, openCodeMaintenanceSmokeControlArg) {
			t.Fatalf("%q: control token survived into %q", args, cleaned)
		}
		if _, err := validateOpenCodeSmokeRequest(cleaned); err != nil {
			t.Fatalf("%q: cleaned argv %q must validate: %v", args, cleaned, err)
		}
	}
	// Absent is absent — an ordinary session is never reported as promoted.
	if _, present := extractOpenCodeMaintenanceSmokeControl([]string{"run", "--format", "json", prompt}); present {
		t.Error("an unpromoted request must not report the control token")
	}
	// And it can never survive into a built argv, because the builder never
	// receives caller tokens.
	for _, a := range buildOpenCodeRunArgs(openCodeRunShapeResume, "ses_1") {
		if a == openCodeMaintenanceSmokeControlArg {
			t.Error("the control token reached a built argv")
		}
	}
}

/* --------------------------------------------------------------------------
   Shared predicates
   -------------------------------------------------------------------------- */

func TestIsOpenCodeTerminalEventType_SharedByBothTransports(t *testing.T) {
	for _, ok := range []string{
		"session.completed", "step.completed", "turn.done", "finish", "session.idle",
		"  Session.Completed  ",
	} {
		if !isOpenCodeTerminalEventType(ok) {
			t.Errorf("%q must close a turn", ok)
		}
	}
	for _, no := range []string{
		"", "text", "message.part.updated", "tool.completed.error",
		"session.error", "error",
	} {
		if isOpenCodeTerminalEventType(no) {
			t.Errorf("%q must not close a turn", no)
		}
	}
}

func TestOpenCodeOptionRejectionText_RecognisesPreInferenceRefusals(t *testing.T) {
	for _, yes := range []string{
		"error: unknown option '--pure'",
		"unknown flag: --pure",
		"unexpected argument '--pure' found",
		"unrecognized option --pure",
		"error: a value is required for '--model'",
	} {
		if !openCodeOptionRejectionText(yes) {
			t.Errorf("%q must read as an option rejection", yes)
		}
	}
	for _, no := range []string{
		"", "provider returned 500", "authentication failed",
		"the model declined to answer",
	} {
		if openCodeOptionRejectionText(no) {
			t.Errorf("%q must not read as an option rejection", no)
		}
	}
}

func openCodeArgvContains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

/* --------------------------------------------------------------------------
   Session-entry gating
   -------------------------------------------------------------------------- */

func TestOpenCodeSessionStartGate_ShowsAndMatchesTheArgvThatWillExec(t *testing.T) {
	// The gate must display and pattern-match exactly what StartSession runs. For
	// a maintenance smoke that is the LADDER's argv, not the wire's — `--pure` is
	// consumed by validation and never reaches a child, so a dialog or an
	// allowlist pattern must not see it either. And the control token must never
	// be rendered to the user.
	dir := t.TempDir()
	al := &AllowList{configPath: filepath.Join(dir, "allow.txt")}
	if err := al.CreateDefault(); err != nil {
		t.Fatalf("CreateDefault: %v", err)
	}
	if err := al.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	prevList := defaultAllowList
	defaultAllowList = al
	t.Cleanup(func() { defaultAllowList = prevList })

	var shown [][]string
	prevDialog := commandApprovalDialogFn
	commandApprovalDialogFn = func(_ string, args []string, _ int) ApprovalResult {
		shown = append(shown, args)
		return ApprovalDeny
	}
	t.Cleanup(func() { commandApprovalDialogFn = prevDialog })

	prompt := openCodeSmokePromptFor(openCodeSmokeMarkerPrefix + "0a1b2c3d")
	wire := append(append([]string{}, openCodeSmokeWireRequests[0]...), prompt)
	promoted := sessionStartArgsForCommand(commandMsg{
		Type: "session_start", Command: "opencode", Args: wire,
	})

	// Unsigned mode reaches the dialog, which is where we can read the argv the
	// gate computed.
	unsigned := &Config{EnableAllowList: true}
	if gateSessionEntryCommand(nil, nil, nil, commandMsg{
		Type: "session_start", Command: "opencode", Args: promoted,
	}, unsigned) {
		t.Fatal("an unsigned session_start must stay dialog-gated")
	}
	if len(shown) != 1 {
		t.Fatalf("expected one dialog, got %d", len(shown))
	}
	want := strings.Join(buildOpenCodeRunArgs(openCodeRunShapeNoSession, ""), " ")
	if got := strings.Join(shown[0], " "); got != want {
		t.Fatalf("dialog argv = %q, want the ladder's %q", got, want)
	}
	for _, a := range shown[0] {
		if a == openCodeMaintenanceSmokeControlArg || a == "--pure" || a == prompt {
			t.Fatalf("dialog argv %q carries %q", shown[0], a)
		}
	}

	// Signed mode auto-approves the synthesized shape, so a headless maintenance
	// run is not parked at a dialog nobody can answer.
	signed := &Config{EnableAllowList: true, CommandSecret: "sec-123"}
	if !gateSessionEntryCommand(nil, nil, nil, commandMsg{
		Type: "session_start", Command: "opencode", Args: promoted,
	}, signed) {
		t.Fatal("a signed maintenance smoke must be auto-approved")
	}
	// The narrow default entry must still not cover a RAW execute of the shape.
	if al.IsAllowed("opencode", buildOpenCodeRunArgs(openCodeRunShapeNoSession, "")) {
		t.Error("the synthesized shape became allowlisted for raw execute")
	}
}
