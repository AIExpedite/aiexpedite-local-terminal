// File: acp_profiles.go
// -----------------------------------------------------------------------------
// OpenCode and Antigravity on the ACP family (L3/L4 spec §2.1, §2.4, §2.5):
// `opencode acp` (OpenCode 2.2.7 and newer) and `agy acp` (agy 1.5.4 and
// newer), each one long-lived child per session, launched WITHOUT
// auto-approve so the CLI asks before risky actions and terminal-service
// relays the question to a person.
//
// They are SELECTED drivers: terminal-service starts a session on one only
// when this computer lists the agent in `capabilities.acpDrivers` (below),
// the CLI version it reports meets the minimum, and its own switch
// (TERMINAL_ACP_DRIVER_AGENTS) names the agent. Every other session runs on
// the one-shot drivers (opencode_native.go, antigravity_native.go), which are
// unchanged.
//
// What a profile owns:
//
//   - argv: exactly `acp`. A START's args are refused, never forwarded: the
//     model and effort travel in the protocol (`session/set_config_option`),
//     and nothing a caller sends may add a flag. agy's
//     `--dangerously-skip-permissions` is refused by name.
//   - the binary: the installed version is re-read at every START; below the
//     minimum the START is refused ACP_CLI_TOO_OLD and the capability is
//     republished. OpenCode's Windows npm launcher (`opencode.cmd`) is the 1.x
//     line and never runs here.
//   - the environment: the one-shot drivers' own sanitiser, plus, for
//     OpenCode, the ask rules (OPENCODE_CONFIG_CONTENT, below) and
//     OPENCODE_DISABLE_AUTOUPDATE (a 6-hour session must not have its binary
//     replaced under it; this child only, the person's own runs keep their
//     setting).
//
// LISTENERS THE CLIS OPEN (owner decision 2026-11-14, "best practice"; spec O2)
//
// Neither driver needs a listener of AI Expedite's: the device speaks to the
// CLI over the child's stdin and stdout only, it never passes a hostname or a
// port, and it never connects to anything the CLI opens. What each CLI opens
// on its own:
//
//   - OpenCode 2.2.7+: `opencode acp` starts a private child server
//     (`opencode serve --stdio --port 0`). It is OpenCode's own: bound to
//     127.0.0.1 only, on a random port, guarded by a 256-bit password minted
//     per launch (OPENCODE_PASSWORD, removed from the environment its tools
//     inherit), and it lives only as long as its stdin, so it ends with the
//     session. Accepted.
//   - OpenCode 1.x: `opencode acp` hosts its server in-process on
//     127.0.0.1:4096 with no password unless the person set one. NEVER run
//     through ACP: the version gate refuses it, and the npm launcher that
//     carries the 1.x line is refused outright.
//   - Antigravity (agy 1.5.4+): `agy acp` runs a language server in-process
//     on two loopback ports of its own (one HTTPS, one HTTP, random per
//     launch). The port check of 2026-11-14 found them answering with NO
//     secret in ACP mode — file read and write, command execution, account
//     details and conversations, outside agy's own permission prompts — while
//     the one-shot `agy` guards the same listeners with a per-launch token
//     (C:/aiexpedite/aixwt/rs/_notes/design/AGY_ACP_PORT_PROBE.md, verdict
//     UNSAFE). The owner accepted that risk on 2026-11-14 with one required
//     mitigation (local/documents/security/ANTIGRAVITY_ACP_RISK_ACCEPTANCE.md,
//     C:/aiexpedite/local/documents/security/ANTIGRAVITY_ACP_RISK_ACCEPTANCE.md):
//     every `agy acp` is launched with ANTIGRAVITY_SERVER_CSRF_TOKEN set to a
//     fresh 32-byte crypto/rand value (newAntigravityCSRFToken), which turns
//     the listeners' token check on. The token goes into agy's child
//     environment only: never argv, never a log line, never a file, never a
//     frame to the server. With the token set, calls over agy's WebSocket
//     bridge without it were refused too (agy 1.5.9, probe §10). What remains
//     accepted: the variable is undocumented (a later agy could ignore it, so
//     each certified agy version is re-checked with the port check),
//     streaming calls, agy's non-RPC web routes and most call types are
//     untested with the token, and any program on the computer can reach
//     loopback ports (with the token it also needs the per-launch secret).
// -----------------------------------------------------------------------------

package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// antigravityACPDriverIncluded decides whether this build runs Antigravity on
// `agy acp` at all. ON by the owner's decision of 2026-11-14, which accepts
// the risk of agy's ACP-mode listeners with the per-launch CSRF token as the
// required mitigation (local/documents/security/ANTIGRAVITY_ACP_RISK_ACCEPTANCE.md;
// evidence: aixwt/rs/_notes/design/AGY_ACP_PORT_PROBE.md; see the header).
//
// To turn it off, set this to false: `antigravity` is then never listed in
// `capabilities.acpDrivers` and an antigravity_acp_start is refused, so every
// Antigravity session stays on the one-shot driver. The server-side switch
// does the same without a release: remove `antigravity` from
// terminal-service's TERMINAL_ACP_DRIVER_AGENTS.
const antigravityACPDriverIncluded = true

// antigravityCSRFTokenEnv is the variable agy's language server reads its
// per-launch token from (AGY_ACP_PORT_PROBE.md §5, §6.1: set, it makes the
// listeners refuse every call without it). Undocumented by agy.
const antigravityCSRFTokenEnv = "ANTIGRAVITY_SERVER_CSRF_TOKEN"

// antigravityExtensionCSRFTokensEnv names further tokens agy would accept;
// an inherited value is removed, so the per-launch token is the only one.
const antigravityExtensionCSRFTokensEnv = "ANTIGRAVITY_EXTENSION_SERVER_CSRF_TOKENS"

// antigravityCSRFTokenBytes is the token's size before encoding: 32 bytes
// (256 bits) from crypto/rand, hex-encoded so it is header-safe.
const antigravityCSRFTokenBytes = 32

// newAntigravityCSRFToken mints one launch's token. A var so a test can make
// the random source fail; nothing else replaces it.
var newAntigravityCSRFToken = func() (string, error) {
	b := make([]byte, antigravityCSRFTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Wire values mirrored from shared-constants 3.23.579 (cliAgentPreference.js
// CLI_AGENT_IDS, opencode.js, antigravity.js, residentKindsWire.js).
// Contract-pinned by acp_profiles_test.go.
const (
	opencodeCliAgentID    = "opencode"
	antigravityCliAgentID = "antigravity"

	// OPENCODE_ACP_MIN_VERSION / ANTIGRAVITY_ACP_MIN_VERSION.
	openCodeACPMinVersion    = "2.2.7"
	antigravityACPMinVersion = "1.5.4"

	// DEVICE_RELAY_CAPABILITIES.ACP_DRIVERS protocol (ACP_DRIVERS_PROTOCOL).
	acpDriversProtocol = 1

	// openCodeConfigContentEnv is the variable OpenCode 2.x layers above the
	// person's global and project config (below managed settings).
	openCodeConfigContentEnv = "OPENCODE_CONFIG_CONTENT"
)

// openCodeACPAskRules is shared-constants OPENCODE_ACP_ASK_RULES: the rules
// the device adds to every `opencode acp`. OpenCode 2.x allows every action
// by default, so without them an ACP session would ask before nothing.
// ORDERED, and OpenCode applies the last matching rule: they ask before edits,
// shell commands, the web, anything outside the folder, Code Mode, image
// generation and every MCP tool (`<server>_<tool>`). Reads, glob, grep,
// skills and questions stay allowed. Pinned byte for byte against
// testdata/opencode_acp_ask_rules.json, a copy of shared-constants
// tests/fixtures/opencodeAcpAskRules.json.
var openCodeACPAskRules = []openCodePermissionRule{
	{Action: "edit", Resource: "*", Effect: "ask"},
	{Action: "shell", Resource: "*", Effect: "ask"},
	{Action: "webfetch", Resource: "*", Effect: "ask"},
	{Action: "websearch", Resource: "*", Effect: "ask"},
	{Action: "external_directory", Resource: "*", Effect: "ask"},
	{Action: "execute", Resource: "*", Effect: "ask"},
	{Action: "experimental_image_generate", Resource: "*", Effect: "ask"},
	{Action: "*_*", Resource: "*", Effect: "ask"},
}

type openCodePermissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

var openCodeACPProfile = &acpAgentProfile{
	cliAgentID:    opencodeCliAgentID,
	noun:          "opencode acp",
	startNoun:     "opencode acp",
	cliName:       "opencode",
	startedOutput: "OpenCode ACP started",
	logTag:        "[opencode-acp]",
	processLabel:  "opencode-acp",
	commandTypes: acpCommandTypes{
		Start: "opencode_acp_start",
		Send:  "opencode_acp_send",
		End:   "opencode_acp_end",
	},
	resultTypes: acpResultTypes{
		Started: "opencode_acp_started",
		Message: "opencode_acp_message",
		Stderr:  "opencode_acp_stderr",
		Error:   "opencode_acp_error",
		Ended:   "opencode_acp_ended",
	},
	buildArgs: func(extraArgs []string, _ ACPStartOptions) ([]string, error) {
		return acpDriverArgs("opencode acp", extraArgs)
	},
	redactArgs:  redactArgs,
	spawnFailed: acpDriverSpawnFailed("opencode acp", "opencode"),
	fenceFrames: true,
	endedOutput: func(exit int) string { return fmt.Sprintf("opencode acp ended (exit code: %d)", exit) },
	firstFrameStall: func(timeout time.Duration) string {
		return fmt.Sprintf("opencode produced no output within %v of starting — it is most likely waiting for a sign-in "+
			"it cannot show here, or wedged at startup (run `opencode` in a terminal on this computer). Session terminated.", timeout)
	},
}

var antigravityACPProfile = &acpAgentProfile{
	cliAgentID:    antigravityCliAgentID,
	noun:          "antigravity acp",
	startNoun:     "agy acp",
	cliName:       "agy",
	startedOutput: "Antigravity ACP started",
	logTag:        "[antigravity-acp]",
	processLabel:  "antigravity-acp",
	commandTypes: acpCommandTypes{
		Start: "antigravity_acp_start",
		Send:  "antigravity_acp_send",
		End:   "antigravity_acp_end",
	},
	resultTypes: acpResultTypes{
		Started: "antigravity_acp_started",
		Message: "antigravity_acp_message",
		Stderr:  "antigravity_acp_stderr",
		Error:   "antigravity_acp_error",
		Ended:   "antigravity_acp_ended",
	},
	buildArgs: func(extraArgs []string, _ ACPStartOptions) ([]string, error) {
		return acpDriverArgs("agy acp", extraArgs)
	},
	redactArgs:  redactArgs,
	spawnFailed: acpDriverSpawnFailed("agy acp", "agy"),
	fenceFrames: true,
	endedOutput: func(exit int) string { return fmt.Sprintf("agy acp ended (exit code: %d)", exit) },
	firstFrameStall: func(timeout time.Duration) string {
		return fmt.Sprintf("agy produced no output within %v of starting — it is most likely waiting for a sign-in "+
			"it cannot show here, or wedged at startup (run `agy` in a terminal on this computer). Session terminated.", timeout)
	},
}

// The launch hooks refer back to their profiles, so they are wired here
// rather than in the profiles' initializers (an initialization cycle).
func init() {
	openCodeACPProfile.prepare = prepareOpenCodeACPLaunch
	antigravityACPProfile.prepare = prepareAntigravityACPLaunch
}

// acpDriverProfiles are the selected ACP drivers this build HAS, in the order
// `capabilities.acpDrivers.agents` lists them.
func acpDriverProfiles() []*acpAgentProfile {
	profiles := []*acpAgentProfile{openCodeACPProfile}
	if antigravityACPDriverIncluded {
		profiles = append(profiles, antigravityACPProfile)
	}
	return profiles
}

// The selected drivers' managers, one per agent, created at agent start
// (agent.go) whether or not the agent is included, so a command for an
// excluded one is still answered (its START is refused) rather than dropped.
var (
	globalOpenCodeACPManager    *ACPManager
	globalAntigravityACPManager *ACPManager
)

// acpDriverManagerFor is the manager of one selected driver (nil before the
// agent initialised it).
func acpDriverManagerFor(p *acpAgentProfile) *ACPManager {
	switch p {
	case openCodeACPProfile:
		return globalOpenCodeACPManager
	case antigravityACPProfile:
		return globalAntigravityACPManager
	}
	return nil
}

// acpDriverManagers are the selected drivers' managers that exist.
func acpDriverManagers() []*ACPManager {
	var out []*ACPManager
	for _, m := range []*ACPManager{globalOpenCodeACPManager, globalAntigravityACPManager} {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}

// acpDriverProfileForCommand is the selected driver a command type belongs to
// (`opencode_acp_*`, `antigravity_acp_*`), or nil.
func acpDriverProfileForCommand(cmdType string) *acpAgentProfile {
	for _, p := range []*acpAgentProfile{openCodeACPProfile, antigravityACPProfile} {
		switch cmdType {
		case p.commandTypes.Start, p.commandTypes.Send, p.commandTypes.End:
			return p
		}
	}
	return nil
}

// acpDriverManagerForCommand is the manager a selected driver's command goes
// to, or nil.
func acpDriverManagerForCommand(cmdType string) *ACPManager {
	if p := acpDriverProfileForCommand(cmdType); p != nil {
		return acpDriverManagerFor(p)
	}
	return nil
}

// acpDriverInstalledVersion is the installed version of an agent that runs on
// a selected driver in this build, when the driver can run it ("" otherwise):
// the follow-up ledger's installed version for OpenCode and Antigravity, whose
// live follow-ups exist only on that driver.
func acpDriverInstalledVersion(cliAgentID string) string {
	for _, p := range acpDriverProfiles() {
		if p.cliAgentID != cliAgentID {
			continue
		}
		if b := readACPDriverBinary(p); acpDriverEligible(p, b) {
			return b.version
		}
	}
	return ""
}

// acpDriverMinVersion is the oldest CLI a selected driver runs.
func acpDriverMinVersion(p *acpAgentProfile) string {
	switch p.cliAgentID {
	case opencodeCliAgentID:
		return openCodeACPMinVersion
	case antigravityCliAgentID:
		return antigravityACPMinVersion
	}
	return ""
}

// acpDriverArgs is the whole argv of a selected driver: `acp`, and nothing a
// START carried. Args are refused rather than dropped, so a caller asking for
// a flag is told no instead of being quietly overruled.
func acpDriverArgs(startNoun string, extraArgs []string) ([]string, error) {
	for _, a := range extraArgs {
		lower := strings.ToLower(strings.TrimSpace(a))
		if lower == "--dangerously-skip-permissions" || lower == "--allow-dangerously-skip-permissions" ||
			strings.HasPrefix(lower, "--dangerously-skip-permissions=") {
			return nil, fmt.Errorf("%s never runs with %s: it asks before risky actions on this driver", startNoun, a)
		}
	}
	if len(extraArgs) > 0 {
		return nil, fmt.Errorf("%s takes no start arguments (got %d): the model and effort are set in the protocol", startNoun, len(extraArgs))
	}
	return []string{"acp"}, nil
}

func acpDriverSpawnFailed(startNoun, command string) func(error) error {
	return func(err error) error {
		return fmt.Errorf("failed to start %s (is `%s` on PATH?): %w", startNoun, command, err)
	}
}

/* --------------------------------------------------------------------------
   The installed CLI: path, version, eligibility
   -------------------------------------------------------------------------- */

// acpDriverBinary is a selected driver's CLI as installed now.
type acpDriverBinary struct {
	path    string
	found   bool
	shim    bool
	version string
	key     versionProbeKey
}

// readACPDriverBinary resolves the profile's CLI the way its one-shot driver
// and the CLI Agents card do, and reads its version through that same cached
// probe (so a steady state spawns no `--version` child). An OpenCode npm
// launcher is never probed: it is the 1.x line, refused whatever it says.
func readACPDriverBinary(p *acpAgentProfile) acpDriverBinary {
	acpDriverBinaryReaderMu.RLock()
	read := acpDriverBinaryReader
	acpDriverBinaryReaderMu.RUnlock()
	return read(p)
}

// acpDriverBinaryReader is readACPDriverBinary's source, swappable by a test
// under its lock (a machine-info gather can read it from another goroutine).
var (
	acpDriverBinaryReaderMu sync.RWMutex
	acpDriverBinaryReader   = readInstalledACPDriverBinary
)

func readInstalledACPDriverBinary(p *acpAgentProfile) acpDriverBinary {
	var path string
	switch p.cliAgentID {
	case opencodeCliAgentID:
		path = resolveOpenCodeExecutable()
	case antigravityCliAgentID:
		path = resolveExecutable("agy")
	default:
		return acpDriverBinary{}
	}
	b := acpDriverBinary{path: path}
	if path == "" || !filepath.IsAbs(path) {
		return b
	}
	if _, err := os.Stat(path); err != nil {
		return b
	}
	b.found = true
	if p.cliAgentID == opencodeCliAgentID && isWindowsShimPath(path) {
		b.shim = true
		return b
	}
	var raw string
	if p.cliAgentID == opencodeCliAgentID {
		raw = openCodeProbeVersion(path)
	} else {
		raw = cachedProbeVersion(path)
	}
	b.version = parseCLIVersionTriple(raw)
	if v, key, ok := peekCachedProbeVersionIdentity(path); ok && parseCLIVersionTriple(v) == b.version {
		b.key = key
	}
	return b
}

// acpDriverEligible reports whether the installed CLI can run on the driver:
// found, not an npm launcher, and at or above the minimum.
func acpDriverEligible(p *acpAgentProfile, b acpDriverBinary) bool {
	minVersion := acpDriverMinVersion(p)
	return b.found && !b.shim && b.version != "" && minVersion != "" && compareSemver(b.version, minVersion) >= 0
}

// prepareACPDriverBinary is the START's half of the version gate: the binary
// about to be launched is re-read, and one below the minimum (an upgrade or
// downgrade since the computer reported its drivers) is refused
// ACP_CLI_TOO_OLD, never launched, and the capability is republished.
func prepareACPDriverBinary(p *acpAgentProfile, displayName string) (acpDriverBinary, error) {
	b := readACPDriverBinary(p)
	if !b.found {
		return b, fmt.Errorf("%s is not installed on this computer (no `%s` on PATH)", displayName, p.cliName)
	}
	if acpDriverEligible(p, b) {
		return b, nil
	}
	scheduleACPDriversRefresh()
	minVersion := acpDriverMinVersion(p)
	msg := fmt.Sprintf("%s %s is older than %s, the oldest that runs on this driver — update %s, or reinstall it", displayName, orUnknown(b.version), minVersion, displayName)
	if b.shim {
		msg = fmt.Sprintf("%s on this computer is the npm launcher (%s), which carries the 1.x line — install %s %s or newer as its native binary", displayName, filepath.Base(b.path), displayName, minVersion)
	}
	return b, &acpStartError{Code: acpErrorCodeCLITooOld, Message: msg}
}

func orUnknown(version string) string {
	if version == "" {
		return "(version unknown)"
	}
	return version
}

func prepareOpenCodeACPLaunch(req acpLaunchRequest) (acpLaunch, error) {
	b, err := prepareACPDriverBinary(openCodeACPProfile, "OpenCode")
	if err != nil {
		return acpLaunch{}, err
	}
	env, err := openCodeACPEnv(os.Environ())
	if err != nil {
		return acpLaunch{}, err
	}
	return acpLaunch{executable: b.path, env: env, version: b.version, versionKey: b.key}, nil
}

func prepareAntigravityACPLaunch(req acpLaunchRequest) (acpLaunch, error) {
	if !antigravityACPDriverIncluded {
		return acpLaunch{}, fmt.Errorf("this desktop app does not run Antigravity on agy acp")
	}
	b, err := prepareACPDriverBinary(antigravityACPProfile, "Antigravity")
	if err != nil {
		return acpLaunch{}, err
	}
	env, err := antigravityACPEnv(os.Environ())
	if err != nil {
		return acpLaunch{}, err
	}
	return acpLaunch{executable: b.path, env: env, version: b.version, versionKey: b.key}, nil
}

// antigravityACPEnv is the `agy acp` child's environment: the one-shot
// driver's sanitiser, no inherited CSRF tokens, and a fresh per-launch
// ANTIGRAVITY_SERVER_CSRF_TOKEN (the required mitigation; see the header).
// A launch whose token cannot be minted is refused: agy's listeners would
// otherwise answer without one.
func antigravityACPEnv(env []string) ([]string, error) {
	env = sanitizeAntigravityEnv(env)
	env = stripEnvNames(env, antigravityExtensionCSRFTokensEnv)
	token, err := newAntigravityCSRFToken()
	if err != nil || token == "" {
		return nil, fmt.Errorf("could not create the per-launch token agy acp needs to protect its local ports: %v", err)
	}
	return setEnvVar(env, antigravityCSRFTokenEnv, token), nil
}

// stripEnvNames removes every entry for the named variables (names compared
// as setEnvVar does).
func stripEnvNames(env []string, names ...string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		drop := false
		for _, n := range names {
			if envNameEqual(name, n) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}

/* --------------------------------------------------------------------------
   OpenCode's environment: the ask rules
   -------------------------------------------------------------------------- */

// openCodeACPEnv is the `opencode acp` child's environment: the one-shot
// driver's sanitiser, self-update off for this child, and
// OPENCODE_CONFIG_CONTENT carrying the ask rules.
func openCodeACPEnv(env []string) ([]string, error) {
	env = sanitizeOpenCodeEnv(env)
	overlay, err := openCodeACPConfigOverlay(lookupEnv(env, openCodeConfigContentEnv))
	if err != nil {
		return nil, err
	}
	env = setEnvVar(env, openCodeConfigContentEnv, overlay)
	env = setEnvVar(env, "OPENCODE_DISABLE_AUTOUPDATE", "true")
	return env, nil
}

// openCodeACPConfigOverlay returns the OPENCODE_CONFIG_CONTENT the child gets:
// the ask rules appended to the `permissions` of the value the person already
// exported (so a `deny` they configured still wins: the last match wins, and
// an ask never outranks a deny), or the rules alone when they exported none.
// A value that cannot take them — not JSONC, not an object, `permissions` not
// a list — is refused ACP_CONFIG_OVERLAY_INVALID; it is never replaced.
func openCodeACPConfigOverlay(existing string) (string, error) {
	config := map[string]json.RawMessage{}
	if strings.TrimSpace(existing) != "" {
		// OpenCode reads its config as JSONC, so the person's value may carry
		// comments and trailing commas.
		if err := json.Unmarshal(stripJSONC([]byte(existing)), &config); err != nil || config == nil {
			return "", &acpStartError{
				Code:    acpErrorCodeConfigOverlayInvalid,
				Message: "OPENCODE_CONFIG_CONTENT on this computer is not a JSON object, so OpenCode's ask-before-acting rules cannot be added to it — fix or unset it to run OpenCode here",
			}
		}
	}
	var rules []json.RawMessage
	if raw, ok := config["permissions"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &rules); err != nil {
			return "", &acpStartError{
				Code:    acpErrorCodeConfigOverlayInvalid,
				Message: "OPENCODE_CONFIG_CONTENT on this computer has a `permissions` that is not a list, so OpenCode's ask-before-acting rules cannot be added to it — fix or unset it to run OpenCode here",
			}
		}
	}
	for _, rule := range openCodeACPAskRules {
		b, err := json.Marshal(rule)
		if err != nil {
			return "", err
		}
		rules = append(rules, b)
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return "", err
	}
	config["permissions"] = encoded
	out, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// lookupEnv is the value of key in env ("" when absent). Windows variable
// names are case-insensitive.
func lookupEnv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		name, value, ok := strings.Cut(env[i], "=")
		if ok && envNameEqual(name, key) {
			return value
		}
	}
	return ""
}

// envNameEqual compares variable names the way setEnvVar does: exactly on
// Unix, case-insensitively on Windows.
func envNameEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

/* --------------------------------------------------------------------------
   capabilities.acpDrivers
   -------------------------------------------------------------------------- */

// acpDriversCapability is DEVICE_RELAY_CAPABILITIES.ACP_DRIVERS: the selected
// ACP drivers this computer can run now. Always reported, never omitted: an
// empty list means "this build has them, the CLI is too old or missing".
type acpDriversCapability struct {
	Protocol int      `json:"protocol"`
	Agents   []string `json:"agents"`
}

// newACPDriversCapability lists every driver this build has whose installed
// CLI meets its minimum.
func newACPDriversCapability() *acpDriversCapability {
	agents := []string{}
	for _, p := range acpDriverProfiles() {
		if acpDriverEligible(p, readACPDriverBinary(p)) {
			agents = append(agents, p.cliAgentID)
		}
	}
	return &acpDriversCapability{Protocol: acpDriversProtocol, Agents: agents}
}

func sameACPDrivers(a, b *acpDriversCapability) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Protocol != b.Protocol || len(a.Agents) != len(b.Agents) {
		return false
	}
	for i := range a.Agents {
		if a.Agents[i] != b.Agents[i] {
			return false
		}
	}
	return true
}

// scheduleACPDriversRefresh runs refreshACPDriversCapability off the caller's
// goroutine: a version probe of another CLI must never hold a START or a
// usage refresh. A var so the test suite can keep it from outliving the test
// that triggered it (TestMain); refreshACPDriversCapability is tested directly.
var scheduleACPDriversRefresh = func() { go refreshACPDriversCapability() }

// refreshACPDriversCapability re-reads the installed CLIs and, when the list
// changed, folds it into the cached MachineInfo and sends it to the backend
// now (the republishRelayFollowUpCapability pattern), rather than leaving
// terminal-service to choose a driver from a 6-hour-old report. Called after
// every CLI usage refresh (which re-reads the versions) and whenever a START
// finds the CLI below its driver's minimum. Reports whether anything changed.
func refreshACPDriversCapability() bool {
	caps := newACPDriversCapability()
	machineInfoMu.Lock()
	if machineInfoCache == nil || machineInfoCache.Capabilities == nil ||
		sameACPDrivers(machineInfoCache.Capabilities.ACPDrivers, caps) {
		machineInfoMu.Unlock()
		return false
	}
	updated := *machineInfoCache
	updatedCaps := *updated.Capabilities
	updatedCaps.ACPDrivers = caps
	updated.Capabilities = &updatedCaps
	machineInfoCache = &updated
	machineInfoMu.Unlock()
	fmt.Printf("%s[acp] ACP drivers now %v%s\n", colorCyan, caps.Agents, colorReset)
	if onMachineCapabilityAdded != nil {
		onMachineCapabilityAdded()
	}
	return true
}
