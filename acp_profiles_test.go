package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// pinACPDriverBinaries makes readACPDriverBinary answer from bins (by CLI
// agent id) for one test; an agent missing from bins reads as not installed.
func pinACPDriverBinaries(t *testing.T, bins map[string]acpDriverBinary) {
	t.Helper()
	acpDriverBinaryReaderMu.Lock()
	prev := acpDriverBinaryReader
	acpDriverBinaryReader = func(p *acpAgentProfile) acpDriverBinary { return bins[p.cliAgentID] }
	acpDriverBinaryReaderMu.Unlock()
	t.Cleanup(func() {
		acpDriverBinaryReaderMu.Lock()
		acpDriverBinaryReader = prev
		acpDriverBinaryReaderMu.Unlock()
	})
}

func installedACPBinary(path, version string) acpDriverBinary {
	return acpDriverBinary{path: path, found: true, version: version}
}

func acpEnvValue(env []string, key string) (string, int) {
	value, n := "", 0
	for _, e := range env {
		if name, v, ok := strings.Cut(e, "="); ok && envNameEqual(name, key) {
			value = v
			n++
		}
	}
	return value, n
}

/* --- contract pins (shared-constants 3.23.579) --- */

func TestACPDriverWireValuesMatchSharedConstants(t *testing.T) {
	cases := map[string][2]string{
		"opencode start":    {openCodeACPProfile.commandTypes.Start, "opencode_acp_start"},
		"opencode send":     {openCodeACPProfile.commandTypes.Send, "opencode_acp_send"},
		"opencode end":      {openCodeACPProfile.commandTypes.End, "opencode_acp_end"},
		"opencode started":  {openCodeACPProfile.resultTypes.Started, "opencode_acp_started"},
		"opencode message":  {openCodeACPProfile.resultTypes.Message, "opencode_acp_message"},
		"opencode stderr":   {openCodeACPProfile.resultTypes.Stderr, "opencode_acp_stderr"},
		"opencode error":    {openCodeACPProfile.resultTypes.Error, "opencode_acp_error"},
		"opencode ended":    {openCodeACPProfile.resultTypes.Ended, "opencode_acp_ended"},
		"agy start":         {antigravityACPProfile.commandTypes.Start, "antigravity_acp_start"},
		"agy send":          {antigravityACPProfile.commandTypes.Send, "antigravity_acp_send"},
		"agy end":           {antigravityACPProfile.commandTypes.End, "antigravity_acp_end"},
		"agy started":       {antigravityACPProfile.resultTypes.Started, "antigravity_acp_started"},
		"agy message":       {antigravityACPProfile.resultTypes.Message, "antigravity_acp_message"},
		"agy stderr":        {antigravityACPProfile.resultTypes.Stderr, "antigravity_acp_stderr"},
		"agy error":         {antigravityACPProfile.resultTypes.Error, "antigravity_acp_error"},
		"agy ended":         {antigravityACPProfile.resultTypes.Ended, "antigravity_acp_ended"},
		"opencode id":       {opencodeCliAgentID, "opencode"},
		"agy id":            {antigravityCliAgentID, "antigravity"},
		"opencode min":      {openCodeACPMinVersion, "2.2.7"},
		"agy min":           {antigravityACPMinVersion, "1.5.4"},
		"too old":           {acpErrorCodeCLITooOld, "ACP_CLI_TOO_OLD"},
		"overlay invalid":   {acpErrorCodeConfigOverlayInvalid, "ACP_CONFIG_OVERLAY_INVALID"},
		"frame refused":     {acpErrorCodeFrameRefused, "ACP_FRAME_REFUSED"},
		"steering":          {openCodeMethodSteering, "_session/steering"},
		"injected":          {openCodeSteerInjected, "injected"},
		"prompt required":   {openCodeSteerPromptRequired, "promptRequired"},
		"next prompt":       {followUpProcessedInNextPrompt, "nextPrompt"},
		"relay turn prefix": {relayTurnPromptRPCIDPrefix, "relay-turn-"},
		"agy follow-up id":  {relayFollowUpPromptRPCID("att-1"), "relay-turn-fu_att-1"},
	}
	for name, c := range cases {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if acpDriversProtocol != 1 {
		t.Errorf("ACP_DRIVERS_PROTOCOL = %d, want 1", acpDriversProtocol)
	}
	raw, _ := json.Marshal(&capabilitiesInfo{ACPDrivers: &acpDriversCapability{Protocol: 1, Agents: []string{}}})
	if !bytes.Contains(raw, []byte(`"acpDrivers":{"protocol":1,"agents":[]}`)) {
		t.Errorf("acpDrivers wire shape: %s", raw)
	}
	// Never omitted: an absent field reads to terminal-service as an agent
	// that does not have the drivers at all.
	if raw, _ := json.Marshal(&capabilitiesInfo{}); !bytes.Contains(raw, []byte(`"acpDrivers":`)) {
		t.Errorf("acpDrivers must never be omitted: %s", raw)
	}
}

// The device's ask rules are shared-constants OPENCODE_ACP_ASK_RULES, byte for
// byte: testdata/opencode_acp_ask_rules.json is a copy of shared-constants
// tests/fixtures/opencodeAcpAskRules.json, which pins the SC side.
func TestOpenCodeACPAskRulesMatchTheSharedFixture(t *testing.T) {
	want, err := os.ReadFile("testdata/opencode_acp_ask_rules.json")
	if err != nil {
		t.Fatal(err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n")) // a Windows checkout
	got, err := json.MarshalIndent(openCodeACPAskRules, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(got)+"\n" != string(want) {
		t.Fatalf("ask rules drifted from the shared fixture:\n%s\nwant\n%s", got, want)
	}
}

/* --- argv --- */

func TestACPDriverArgvIsExactlyACP(t *testing.T) {
	for _, p := range []*acpAgentProfile{openCodeACPProfile, antigravityACPProfile} {
		args, err := p.buildArgs(nil, ACPStartOptions{})
		if err != nil || len(args) != 1 || args[0] != "acp" {
			t.Fatalf("%s argv = %v, %v; want [acp]", p.noun, args, err)
		}
		args, err = p.buildArgs([]string{}, ACPStartOptions{AllowAlwaysApprove: true, AllowAPIKeyFallback: true})
		if err != nil || len(args) != 1 || args[0] != "acp" {
			t.Fatalf("%s argv with Grok's opt-ins = %v, %v; want [acp]", p.noun, args, err)
		}
	}
}

func TestACPDriverRefusesEveryStartArg(t *testing.T) {
	for _, p := range []*acpAgentProfile{openCodeACPProfile, antigravityACPProfile} {
		for _, extra := range [][]string{{"--model", "x"}, {"--port", "4096"}, {"--hostname", "0.0.0.0"}, {"serve"}} {
			if args, err := p.buildArgs(extra, ACPStartOptions{}); err == nil {
				t.Errorf("%s accepted start args %v as %v", p.noun, extra, args)
			}
		}
	}
}

func TestAntigravityACPRefusesSkipPermissionsByName(t *testing.T) {
	for _, flag := range []string{"--dangerously-skip-permissions", "--allow-dangerously-skip-permissions", "--DANGEROUSLY-SKIP-PERMISSIONS"} {
		_, err := antigravityACPProfile.buildArgs([]string{flag}, ACPStartOptions{})
		if err == nil || !strings.Contains(err.Error(), "never runs with") {
			t.Errorf("%s: %v, want a refusal naming the flag", flag, err)
		}
	}
}

/* --- OpenCode's environment --- */

func TestOpenCodeACPEnvCarriesTheAskRules(t *testing.T) {
	env, err := openCodeACPEnv([]string{"PATH=/usr/bin", "ANTHROPIC_API_KEY=strip-me"})
	if err != nil {
		t.Fatal(err)
	}
	content, n := acpEnvValue(env, openCodeConfigContentEnv)
	if n != 1 {
		t.Fatalf("%s set %d times", openCodeConfigContentEnv, n)
	}
	var cfg struct {
		Permissions []openCodePermissionRule `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Permissions) != len(openCodeACPAskRules) {
		t.Fatalf("permissions %v", cfg.Permissions)
	}
	for i, r := range cfg.Permissions {
		if r != openCodeACPAskRules[i] {
			t.Fatalf("rule %d = %+v, want %+v", i, r, openCodeACPAskRules[i])
		}
	}
	if v, _ := acpEnvValue(env, "OPENCODE_DISABLE_AUTOUPDATE"); v != "true" {
		t.Errorf("OPENCODE_DISABLE_AUTOUPDATE = %q", v)
	}
	if _, n := acpEnvValue(env, "ANTHROPIC_API_KEY"); n != 0 {
		t.Error("the one-shot driver's sanitiser was not applied")
	}
}

// A value the person exported keeps every key and every rule it had; the ask
// rules come AFTER its rules, so a deny of theirs still wins (an ask never
// outranks a deny, and the last match wins for the rest).
func TestOpenCodeACPEnvAppendsToTheExportedValue(t *testing.T) {
	exported := `{
	  // the person's own config, JSONC as OpenCode reads it
	  "model": "anthropic/claude-x",
	  "permissions": [
	    { "action": "shell", "resource": "git push *", "effect": "deny" },
	  ],
	}`
	env, err := openCodeACPEnv([]string{openCodeConfigContentEnv + "=" + exported})
	if err != nil {
		t.Fatal(err)
	}
	content, n := acpEnvValue(env, openCodeConfigContentEnv)
	if n != 1 {
		t.Fatalf("set %d times", n)
	}
	var cfg struct {
		Model       string                   `json:"model"`
		Permissions []openCodePermissionRule `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(content), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "anthropic/claude-x" {
		t.Errorf("the person's model was dropped: %s", content)
	}
	if len(cfg.Permissions) != 1+len(openCodeACPAskRules) ||
		cfg.Permissions[0] != (openCodePermissionRule{Action: "shell", Resource: "git push *", Effect: "deny"}) ||
		cfg.Permissions[1] != openCodeACPAskRules[0] {
		t.Fatalf("permissions = %+v", cfg.Permissions)
	}
}

func TestOpenCodeACPEnvRefusesAValueItCannotExtend(t *testing.T) {
	for name, exported := range map[string]string{
		"not JSON":                `permissions = ask`,
		"not an object":           `[{"action":"edit","resource":"*","effect":"allow"}]`,
		"null":                    `null`,
		"permissions not a list":  `{"permissions": {"edit": "allow"}}`,
		"permissions not a list2": `{"permissions": "allow"}`,
	} {
		_, err := openCodeACPEnv([]string{openCodeConfigContentEnv + "=" + exported})
		if acpTypedErrorCode(err) != acpErrorCodeConfigOverlayInvalid {
			t.Errorf("%s: %v, want %s", name, err, acpErrorCodeConfigOverlayInvalid)
		}
	}
}

/* --- Antigravity's environment: the required mitigation --- */

func TestAntigravityACPLaunchCarriesAFreshCSRFToken(t *testing.T) {
	pinACPDriverBinaries(t, map[string]acpDriverBinary{antigravityCliAgentID: installedACPBinary("/opt/agy", "1.5.9")})
	launch, err := prepareAntigravityACPLaunch(acpLaunchRequest{ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	token, n := acpEnvValue(launch.env, antigravityCSRFTokenEnv)
	if n != 1 {
		t.Fatalf("%s set %d times in agy acp's environment", antigravityCSRFTokenEnv, n)
	}
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != antigravityCSRFTokenBytes || antigravityCSRFTokenBytes != 32 {
		t.Fatalf("token is not 32 random bytes, hex-encoded: %q (%v)", token, err)
	}
	args, _ := antigravityACPProfile.buildArgs(nil, ACPStartOptions{})
	for _, a := range append(args, launch.executable) {
		if strings.Contains(a, token) {
			t.Fatal("the token reached the command line")
		}
	}
}

func TestAntigravityACPLaunchesNeverReuseAToken(t *testing.T) {
	pinACPDriverBinaries(t, map[string]acpDriverBinary{antigravityCliAgentID: installedACPBinary("/opt/agy", "1.5.9")})
	t.Setenv(antigravityCSRFTokenEnv, "inherited-token")
	t.Setenv(antigravityExtensionCSRFTokensEnv, "extra-token")
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		launch, err := prepareAntigravityACPLaunch(acpLaunchRequest{ID: "s"})
		if err != nil {
			t.Fatal(err)
		}
		token, n := acpEnvValue(launch.env, antigravityCSRFTokenEnv)
		if n != 1 || token == "" || token == "inherited-token" {
			t.Fatalf("launch %d: token %q set %d times", i, token, n)
		}
		if seen[token] {
			t.Fatalf("launch %d reused a token", i)
		}
		seen[token] = true
		if _, n := acpEnvValue(launch.env, antigravityExtensionCSRFTokensEnv); n != 0 {
			t.Fatal("an inherited extension token would also open agy's ports")
		}
	}
}

func TestAntigravityACPLaunchIsRefusedWithoutAToken(t *testing.T) {
	pinACPDriverBinaries(t, map[string]acpDriverBinary{antigravityCliAgentID: installedACPBinary("/opt/agy", "1.5.9")})
	prev := newAntigravityCSRFToken
	newAntigravityCSRFToken = func() (string, error) { return "", errors.New("no entropy") }
	t.Cleanup(func() { newAntigravityCSRFToken = prev })
	if _, err := prepareAntigravityACPLaunch(acpLaunchRequest{ID: "s"}); err == nil {
		t.Fatal("agy acp would have started with its ports open")
	}
}

/* --- the version gate --- */

func TestACPDriverEligibility(t *testing.T) {
	cases := []struct {
		name string
		p    *acpAgentProfile
		b    acpDriverBinary
		want bool
	}{
		{"opencode 2.2.7", openCodeACPProfile, installedACPBinary("/bin/opencode", "2.2.7"), true},
		{"opencode 2.3.0", openCodeACPProfile, installedACPBinary("/bin/opencode", "2.3.0"), true},
		{"opencode 2.2.6", openCodeACPProfile, installedACPBinary("/bin/opencode", "2.2.6"), false},
		{"opencode 1.19.5 (in-process unauthenticated listener)", openCodeACPProfile, installedACPBinary("/bin/opencode", "1.19.5"), false},
		{"opencode unreadable version", openCodeACPProfile, installedACPBinary("/bin/opencode", ""), false},
		{"opencode npm launcher", openCodeACPProfile, acpDriverBinary{path: `C:\npm\opencode.cmd`, found: true, shim: true, version: "9.9.9"}, false},
		{"opencode missing", openCodeACPProfile, acpDriverBinary{}, false},
		{"agy 1.5.4", antigravityACPProfile, installedACPBinary("/bin/agy", "1.5.4"), true},
		{"agy 1.5.9", antigravityACPProfile, installedACPBinary("/bin/agy", "1.5.9"), true},
		{"agy 1.5.3", antigravityACPProfile, installedACPBinary("/bin/agy", "1.5.3"), false},
		{"agy 1.5.0", antigravityACPProfile, installedACPBinary("/bin/agy", "1.5.0"), false},
	}
	for _, c := range cases {
		if got := acpDriverEligible(c.p, c.b); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestACPDriverStartRefusesACLIBelowTheMinimum(t *testing.T) {
	pinACPDriverBinaries(t, map[string]acpDriverBinary{
		opencodeCliAgentID:    installedACPBinary("/bin/opencode", "1.19.5"),
		antigravityCliAgentID: installedACPBinary("/bin/agy", "1.5.3"),
	})
	if _, err := prepareOpenCodeACPLaunch(acpLaunchRequest{}); acpTypedErrorCode(err) != acpErrorCodeCLITooOld {
		t.Errorf("opencode 1.19.5: %v, want %s", err, acpErrorCodeCLITooOld)
	}
	if _, err := prepareAntigravityACPLaunch(acpLaunchRequest{}); acpTypedErrorCode(err) != acpErrorCodeCLITooOld {
		t.Errorf("agy 1.5.3: %v, want %s", err, acpErrorCodeCLITooOld)
	}
	pinACPDriverBinaries(t, map[string]acpDriverBinary{
		opencodeCliAgentID: {path: `C:\npm\opencode.cmd`, found: true, shim: true},
	})
	_, err := prepareOpenCodeACPLaunch(acpLaunchRequest{})
	if acpTypedErrorCode(err) != acpErrorCodeCLITooOld || !strings.Contains(err.Error(), "npm launcher") {
		t.Errorf("npm launcher: %v", err)
	}
	pinACPDriverBinaries(t, nil)
	if _, err := prepareOpenCodeACPLaunch(acpLaunchRequest{}); err == nil || acpTypedErrorCode(err) != "" ||
		!strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing opencode: %v", err)
	}
}

func TestACPDriverCommandRouting(t *testing.T) {
	for _, p := range []*acpAgentProfile{openCodeACPProfile, antigravityACPProfile} {
		for _, typ := range []string{p.commandTypes.Start, p.commandTypes.Send, p.commandTypes.End} {
			if acpDriverProfileForCommand(typ) != p || !isACPFamilyCommand(typ) {
				t.Errorf("%s does not route to %s", typ, p.noun)
			}
			if got := rejectionResultType(typ); got != p.resultTypes.Error {
				t.Errorf("rejection type for %s = %s", typ, got)
			}
		}
		if !isWorkContinuationCommand(commandMsg{Type: p.commandTypes.Send}) || !isWorkContinuationCommand(commandMsg{Type: p.commandTypes.End}) {
			t.Errorf("%s SEND / END must stay deliverable while draining", p.noun)
		}
		if isWorkContinuationCommand(commandMsg{Type: p.commandTypes.Start}) {
			t.Errorf("%s START must be refused while draining", p.noun)
		}
		if !isSessionEndCommandType(p.commandTypes.End) {
			t.Errorf("%s END must pass a fenced session", p.noun)
		}
	}
	if p, _ := acpFamilyRoute("grok_acp_send"); p != grokACPProfile {
		t.Error("grok_acp_send left Grok's profile")
	}
	if acpDriverProfileForCommand("grok_acp_start") != nil || acpDriverProfileForCommand("opencode_native_start") != nil {
		t.Error("only the selected drivers' commands are theirs")
	}
}

// A signed START needs no allowlist entry: the profile owns the argv.
func TestACPDriverSignedStartSkipsTheAllowlist(t *testing.T) {
	cfg := &Config{CommandSecret: "secret", EnableAllowList: true}
	prevList := defaultAllowList
	defaultAllowList = &AllowList{}
	t.Cleanup(func() { defaultAllowList = prevList })
	prevDialog := commandApprovalDialogFn
	commandApprovalDialogFn = func(string, []string, int) ApprovalResult {
		t.Fatal("a signed ACP start showed the approval dialog")
		return ApprovalDeny
	}
	t.Cleanup(func() { commandApprovalDialogFn = prevDialog })
	for _, p := range []*acpAgentProfile{openCodeACPProfile, antigravityACPProfile} {
		if !gateSessionEntryCommand(nil, nil, nil, commandMsg{Type: p.commandTypes.Start, SessionID: "s"}, cfg) {
			t.Errorf("%s signed start was gated", p.noun)
		}
	}
}

// Unsigned, the person sees exactly the argv that will run, and "Always"
// never persists a blanket `opencode *` / `agy *`.
func TestACPDriverUnsignedStartShowsTheDialogAndNeverPersists(t *testing.T) {
	cfg := &Config{EnableAllowList: true, ApprovalTimeoutSec: 1}
	prevList := defaultAllowList
	defaultAllowList = &AllowList{}
	t.Cleanup(func() { defaultAllowList = prevList })
	var shown [][]string
	prevDialog := commandApprovalDialogFn
	commandApprovalDialogFn = func(cmd string, args []string, _ int) ApprovalResult {
		shown = append(shown, append([]string{cmd}, args...))
		return ApprovalAlways
	}
	t.Cleanup(func() { commandApprovalDialogFn = prevDialog })
	for _, p := range []*acpAgentProfile{openCodeACPProfile, antigravityACPProfile} {
		if !gateSessionEntryCommand(nil, nil, nil, commandMsg{Type: p.commandTypes.Start, SessionID: "s", Args: []string{"--x"}}, cfg) {
			t.Errorf("%s approved start refused", p.noun)
		}
	}
	if len(shown) != 2 || strings.Join(shown[0], " ") != "opencode acp" || strings.Join(shown[1], " ") != "agy acp" {
		t.Fatalf("dialogs showed %v", shown)
	}
	if defaultAllowList.IsAllowed("opencode", []string{"run"}) || defaultAllowList.IsAllowed("agy", []string{"--print"}) {
		t.Fatal("an Always click persisted a blanket pattern")
	}
}
