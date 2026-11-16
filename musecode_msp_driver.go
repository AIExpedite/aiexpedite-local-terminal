// File: musecode_msp_driver.go
// -----------------------------------------------------------------------------
// Muse Code on a long-lived `muse serve` chat host (L5 spec §2): one host per
// AI Expedite session, speaking the Muse Session Protocol (MSP, newline-
// delimited JSON-RPC 2.0) over its stdio, on the ACP family's transport
// (acp_family.go) with a profile of its own. It gives Muse Code what Claude
// Code and Grok Build already have: Muse asks before risky actions and a
// person answers, and a reconnect resumes the exact conversation.
//
// It is a SELECTED driver: terminal-service starts a session on it only when
// this computer lists `museCode` in `capabilities.mspDrivers` (below), the
// Muse Code version it reports is 1.4.18 or newer, and its own switch
// (TERMINAL_ACP_DRIVER_AGENTS) names `museCode`. Every other Muse session runs
// on the one-shot `muse exec` driver (musecode_native.go), which is unchanged,
// and so is Ship's raw path (a headless run has no person to answer).
//
// What the profile owns:
//
//   - The binary: the versioned build beside the launcher
//     (museCodeServeBinary: `muse-bin-<version>[.exe]`), never `muse.cmd`, so
//     the launcher's PowerShell hop and self-update never run under a resident
//     host; the version is the build's own. A Windows launcher with no build
//     beside it is not run here.
//   - argv: exactly `serve --disable-request-user-input` (Muse asks its
//     questions in the reply; the client also declares no dialogs). A START's
//     args are refused, never forwarded: the model and effort travel in the
//     protocol. Never on argv: `--no-session-log` (a memory-only host cannot
//     resume), `--disable-write` / `--disable-shell` (the usage probe host's
//     limits, musecode_msp.go: this host must be able to work),
//     `--disable-sandbox`, `--sandbox-network`, `--trust-workspace`,
//     `--enable-attach`, `--listen`, `--join`, `--per-session`, `--provider`,
//     `--base-url`, `--model`, `--preset`, `-c`.
//   - The gate: installed Muse Code 1.4.18 or newer (shared-constants
//     MUSE_CODE_MSP_MIN_VERSION, the release whose MSP behaviour was checked)
//     AND its own schema export has the surface the driver uses
//     (musecode_msp_surface.go). Re-read at every START; one that no longer
//     qualifies is refused ACP_CLI_TOO_OLD and the capability republished.
//   - The environment: the one-shot driver's own sanitiser (other agents'
//     credentials are stripped).
//   - End: close stdin, wait at most 5 s, then kill the process TREE (a host
//     may have shells running under it). The child gets its own process group
//     on macOS and Linux so that kill reaches them.
//
// APPROVALS (owner decisions O1, O2; L5 spec §2.3, §7.1)
//
// The chat host runs in approval mode `promptUnmatched`: Muse asks for every
// action no rule allows. The mode is selected on the wire, never on argv, and
// the client-frame fence (msp_fence.go) refuses a `session/start` or
// `session/setApprovalMode` naming any other mode, and any approval answer
// but a once-scoped approve or deny of a request the host actually made. The
// person's OWN Muse permission profile and Muse's approval judge may still
// allow an action without asking: that is the person's own setting, accepted
// as their consent (O2), and Muse reports who decided (`approval/resolved`
// `resolvedBy`).
//
// SANDBOX
//
// Whether the host can sandbox its shell is Muse's to say, in its
// `initialize` answer (`sandboxAvailability`). Muse 1.4.18 and 1.4.20 on
// Windows answer "unavailable" (with no reason), although
// `muse sandbox windows check` reports ready. The device forwards that answer
// verbatim and logs what the host reported; nothing on the device claims the
// sandbox is on.
//
// LISTENERS (checked 2026-11-14, Muse Code 1.4.20 on Windows, the exact chat
// argv, Meta provider and echo provider)
//
// `muse serve` (no `--listen`, `--enable-attach`, `--join`) opened NO
// listening socket and started no child process: none after `initialize`,
// none after `session/start`, none during or after a turn. The only socket
// was the host's own OUTBOUND HTTPS connection to Meta's API (port 443) while
// a model call ran; the echo provider made none. The client (this device) is
// its only connection, over the child's stdin and stdout. AI Expedite never
// passes a hostname or a port and connects to nothing the CLI opens. Not
// checked (the account's quota was spent): a turn running a sandboxed shell,
// whose `--sandbox-network` proxy might listen on loopback while it runs; the
// certification run (relay_follow_up_musecode_certification_test.go) records
// it. Both hosts exited on their own within 5 s of stdin closing; the
// design pass saw 1.4.18 hosts that did not, which is why End kills the tree.
// 1.4.20 does not always exit either (2026-11-15, Windows): of six hosts
// probed with no turn, three outlived their closed stdin by a minute or more
// (two still running when killed at 56 s and 60 s, one gone at 76 s) and three
// went within 0.3 s; and a dev chat host outlived End's 5 s after a refused
// turn and was killed (exit code 1). The ended frame then waits for the
// session's media scan, which reads several folders at once
// (artifact_scan_walk.go) because that scan, not the host, was most of that
// session's 42 s end.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Wire values mirrored from shared-constants 3.23.580 (cliAgentPreference.js
// CLI_AGENT_IDS, museCode.js, residentKindsWire.js). Contract-pinned by
// musecode_msp_driver_test.go.
const (
	// CLI_AGENT_IDS.MUSE_CODE.
	museCodeCliAgentID = "museCode"
	// MUSE_CODE_MSP_MIN_VERSION.
	museCodeMSPMinVersion = "1.4.18"
	// MSP_DRIVERS_PROTOCOL: DEVICE_RELAY_CAPABILITIES.MSP_DRIVERS's protocol.
	mspDriversProtocol = 1
	// MUSE_MSP_CHAT_APPROVAL_MODE.
	museMSPChatApprovalMode = "promptUnmatched"
	// RELAY_MSP_RPC_IDS.INITIALIZE: the handshake's `initialize`.
	relayMSPInitializeRPCID = "relay-msp-init"
)

// museCodeMSPArgs is the chat host's whole argv.
var museCodeMSPArgs = []string{"serve", "--disable-request-user-input"}

// museCodeMSPRefusedFlags are refused by name when a START carries one (every
// START arg is refused; these say why).
var museCodeMSPRefusedFlags = []string{
	"--no-session-log", "--disable-write", "--disable-shell", "--disable-sandbox",
	"--sandbox-network", "--trust-workspace", "--enable-attach", "--listen", "--join",
	"--per-session", "--provider", "--base-url", "--model", "--preset", "--config", "-c",
	"--allow-workspace-trust", "--allow-session-posture",
}

var museCodeMSPProfile = &acpAgentProfile{
	cliAgentID:    museCodeCliAgentID,
	protocol:      acpProtocolMSP,
	noun:          "musecode msp",
	startNoun:     "muse serve",
	cliName:       "muse",
	startedOutput: "Muse Code MSP started",
	logTag:        "[musecode-msp]",
	processLabel:  "musecode-msp",
	commandTypes: acpCommandTypes{
		Start: "musecode_msp_start",
		Send:  "musecode_msp_send",
		End:   "musecode_msp_end",
	},
	resultTypes: acpResultTypes{
		Started: "musecode_msp_started",
		Message: "musecode_msp_message",
		Stderr:  "musecode_msp_stderr",
		Error:   "musecode_msp_error",
		Ended:   "musecode_msp_ended",
	},
	buildArgs: func(extraArgs []string, _ ACPStartOptions) ([]string, error) {
		return museCodeMSPDriverArgs(extraArgs)
	},
	redactArgs:      redactArgs,
	spawnFailed:     acpDriverSpawnFailed("muse serve", "muse"),
	fenceFrames:     true,
	killProcessTree: true,
	endedOutput:     func(exit int) string { return fmt.Sprintf("muse serve ended (exit code: %d)", exit) },
}

func init() {
	museCodeMSPProfile.prepare = prepareMuseCodeMSPLaunch
}

// globalMuseCodeMSPManager runs Muse Code's MSP driver (agent.go). Created
// whether or not this computer qualifies, so a command for it is answered
// (its START refused) rather than dropped.
var globalMuseCodeMSPManager *ACPManager

// museCodeMSPDriverArgs is the whole argv of the chat host. Args are refused
// rather than dropped: a caller asking for a flag is told no.
func museCodeMSPDriverArgs(extraArgs []string) ([]string, error) {
	for _, a := range extraArgs {
		name, _, _ := strings.Cut(strings.TrimSpace(a), "=")
		if containsString(museCodeMSPRefusedFlags, strings.ToLower(name)) {
			return nil, fmt.Errorf("muse serve never runs with %s on this driver", name)
		}
	}
	if len(extraArgs) > 0 {
		return nil, fmt.Errorf("muse serve takes no start arguments (got %d): the model and effort are set in the protocol", len(extraArgs))
	}
	return append([]string(nil), museCodeMSPArgs...), nil
}

/* --------------------------------------------------------------------------
   The installed Muse: build, version, eligibility
   -------------------------------------------------------------------------- */

// mspDriverBinary is Muse Code as installed now, for the MSP driver.
type mspDriverBinary struct {
	// path is the binary the host runs: the versioned build beside the
	// launcher, or the CLI itself on another install layout.
	path  string
	found bool
	// shim: a Windows `.cmd` / `.bat` launcher with no build beside it.
	shim    bool
	version string
	key     versionProbeKey
	// surface: the binary's own schema export has the MSP surface.
	surface bool
}

// mspDriverBinaryReader is readMSPDriverBinary's source, swappable by a test
// under its lock (a machine-info gather can read it from another goroutine).
var (
	mspDriverBinaryReaderMu sync.RWMutex
	mspDriverBinaryReader   = readInstalledMSPDriverBinary
)

func readMSPDriverBinary() mspDriverBinary {
	mspDriverBinaryReaderMu.RLock()
	read := mspDriverBinaryReader
	mspDriverBinaryReaderMu.RUnlock()
	return read()
}

// readInstalledMSPDriverBinary resolves Muse Code the way the one-shot driver
// does, picks the binary the host would run, and reads its version: from the
// build's own name on a launcher install (`muse-bin-1.4.20-R8619.1.exe`), else
// through the cached `--version` probe. The schema is probed only for a build
// at or above the minimum.
func readInstalledMSPDriverBinary() mspDriverBinary {
	launcher := resolveMuseCodeExecutable()
	if launcher == "" || !filepath.IsAbs(launcher) {
		return mspDriverBinary{path: launcher}
	}
	if _, err := os.Stat(launcher); err != nil {
		return mspDriverBinary{path: launcher}
	}
	binary := museCodeServeBinary(launcher)
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return mspDriverBinary{path: binary}
	}
	b := mspDriverBinary{path: binary, found: true}
	if isWindowsShimPath(binary) {
		b.shim = true
		return b
	}
	if binary != launcher {
		b.version = museCodeBuildVersion(binary)
		b.key = versionProbeKey{Path: binary, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}
	} else {
		b.version = parseCLIVersionTriple(cachedProbeVersion(binary))
		if v, key, ok := peekCachedProbeVersionIdentity(binary); ok && parseCLIVersionTriple(v) == b.version {
			b.key = key
		}
	}
	if b.version != "" && compareSemver(b.version, museCodeMSPMinVersion) >= 0 {
		b.surface = museCodeMSPSurfaceOK(binary)
	}
	return b
}

// museCodeBuildVersion is the major.minor.patch a launcher build's own file
// name carries (`muse-bin-1.4.20-R8619.1[.exe]` → 1.4.20), "" otherwise.
func museCodeBuildVersion(binary string) string {
	name := strings.TrimSuffix(filepath.Base(binary), ".exe")
	rest, ok := strings.CutPrefix(name, "muse-bin-")
	if !ok {
		return ""
	}
	return parseCLIVersionTriple(rest)
}

// mspDriverEligible reports whether installed Muse Code can run the MSP
// driver: found, not a bare Windows launcher, at or above the minimum, and its
// schema has the surface.
func mspDriverEligible(b mspDriverBinary) bool {
	return b.found && !b.shim && b.version != "" && compareSemver(b.version, museCodeMSPMinVersion) >= 0 && b.surface
}

// mspDriverInstalledVersion is the installed Muse Code version when the MSP
// driver can run it, else "": the follow-up ledger's installed version for
// Muse Code, whose live follow-ups exist only on this driver.
func mspDriverInstalledVersion() string {
	if b := readMSPDriverBinary(); mspDriverEligible(b) {
		return b.version
	}
	return ""
}

// prepareMSPDriverBinary is the START's half of the gate: the binary about to
// be launched is re-read, and one that no longer qualifies (an upgrade,
// downgrade or uninstall since the computer reported its drivers) is refused
// ACP_CLI_TOO_OLD, never launched, and the capability is republished. Every
// refusal is typed, missing Muse included, so terminal-service settles the
// start from the code.
func prepareMSPDriverBinary() (mspDriverBinary, error) {
	b := readMSPDriverBinary()
	if mspDriverEligible(b) {
		return b, nil
	}
	scheduleACPDriversRefresh()
	var msg string
	switch {
	case !b.found:
		msg = fmt.Sprintf("Muse Code is not installed on this computer (no `muse` on PATH) — install Muse Code %s or newer", museCodeMSPMinVersion)
	case b.shim:
		msg = fmt.Sprintf("Muse Code on this computer is a launcher (%s) with no versioned build beside it — reinstall Muse Code %s or newer", filepath.Base(b.path), museCodeMSPMinVersion)
	case b.version == "" || compareSemver(b.version, museCodeMSPMinVersion) < 0:
		msg = fmt.Sprintf("Muse Code %s is older than %s, the oldest that runs on this driver — update Muse Code, or reinstall it", orUnknown(b.version), museCodeMSPMinVersion)
	default:
		msg = fmt.Sprintf("Muse Code %s does not have the session protocol this driver needs (its schema export lacks part of it) — update Muse Code, or reinstall it", b.version)
	}
	return b, &acpStartError{Code: acpErrorCodeCLITooOld, Message: msg}
}

func prepareMuseCodeMSPLaunch(req acpLaunchRequest) (acpLaunch, error) {
	b, err := prepareMSPDriverBinary()
	if err != nil {
		return acpLaunch{}, err
	}
	return acpLaunch{
		executable: b.path,
		env:        stripEnvPrefixes(os.Environ(), museCodeUnrelatedStripped),
		version:    b.version,
		versionKey: b.key,
		// Pinned before the spawn: the host bills the account it starts with.
		observe: newMuseCodeMSPObserver(req.ID, museCodeUsageAccountKey(currentMuseCodeAccountFingerprint())),
	}, nil
}

/* --------------------------------------------------------------------------
   What the host's own frames tell the device (never edited, never held)
   -------------------------------------------------------------------------- */

// newMuseCodeMSPObserver reads a host's stdout frames for what the device
// keeps of its own, exactly as the one-shot driver's run capture does
// (cliagent_usage_musecode_run.go, musecode_auth_signal.go):
//
//   - `usage/changed`: the account's quota, filed under the account the host
//     was launched with and only while that account is still signed in, so a
//     chat session keeps the CLI Agents card current with no paid Refresh;
//   - `turn/completed`: a turn Muse refused for sign-in (`error.kind`
//     `authRequired`) marks Muse Code signed out; a completed turn signs it in.
//     Both only while the launch account is still the one signed in: a host
//     lives for hours, and after a login switch its turn speaks for the
//     account it started with, not the one the computer uses now;
//   - the `initialize` answer: the host's own sandbox report, logged as Muse
//     gave it.
func newMuseCodeMSPObserver(sessionID, accountKey string) func(line string) {
	return func(line string) {
		f, ok := parseJSONRPCFrame(line)
		if !ok {
			return
		}
		switch {
		case f.Method == mspNotifyUsageChanged:
			reading, ok := parseMuseCodeSubscriptionUsage(f.Params)
			if !ok || !museCodeAccountUnchanged(accountKey) {
				return
			}
			reading.AccountKey = accountKey
			saveMuseCodeUsageLiveIfNewer(reading)
		case f.Method == mspNotifyTurnCompleted:
			var p struct {
				Terminal string `json:"terminal"`
				Error    struct {
					Kind string `json:"kind"`
				} `json:"error"`
			}
			if json.Unmarshal(f.Params, &p) != nil || !museCodeAccountUnchanged(accountKey) {
				return
			}
			now := time.Now()
			switch {
			case p.Error.Kind == mspTurnErrorAuthRequired:
				home, _ := os.UserHomeDir()
				recordMuseCodeSignedOut(now, home, os.Getenv)
			case p.Terminal == mspTurnTerminalCompleted:
				recordMuseCodeSignedIn(now)
			}
		case f.isResponse():
			if id, ok := f.stringID(); ok && id == relayMSPInitializeRPCID && len(f.Result) > 0 {
				logMuseCodeMSPSandbox(sessionID, f.Result)
			}
		}
	}
}

// logMuseCodeMSPSandbox logs the host's sandbox report as Muse gave it.
func logMuseCodeMSPSandbox(sessionID string, result json.RawMessage) {
	var r struct {
		ServerInfo struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
		SandboxAvailability      string `json:"sandboxAvailability"`
		SandboxUnavailableReason string `json:"sandboxUnavailableReason"`
	}
	if json.Unmarshal(result, &r) != nil {
		return
	}
	report := r.SandboxAvailability
	if report == "" {
		report = "not reported"
	}
	if r.SandboxUnavailableReason != "" {
		report += " (" + r.SandboxUnavailableReason + ")"
	}
	fmt.Printf("%s[musecode-msp] Session %s: muse serve %s reports its shell sandbox: %s%s\n",
		colorCyan, sessionID, orUnknown(r.ServerInfo.Version), report, colorReset)
}

/* --------------------------------------------------------------------------
   capabilities.mspDrivers
   -------------------------------------------------------------------------- */

// mspDriversCapability is DEVICE_RELAY_CAPABILITIES.MSP_DRIVERS: the same
// shape as acpDrivers. Always reported, never omitted: an empty list means
// "this build has the driver, the CLI does not qualify".
type mspDriversCapability = acpDriversCapability

// newMSPDriversCapability lists `museCode` when installed Muse Code qualifies.
func newMSPDriversCapability() *mspDriversCapability {
	agents := []string{}
	if mspDriverEligible(readMSPDriverBinary()) {
		agents = append(agents, museCodeCliAgentID)
	}
	return &mspDriversCapability{Protocol: mspDriversProtocol, Agents: agents}
}
