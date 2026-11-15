package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
)

// mockMSPDriverMode makes the test binary a minimal `muse`: `schema
// generate-json-schema --out DIR` copies the file mockMSPSchemaEnv names into
// DIR/msp.schema.json; `serve` answers `initialize` with what it was launched
// with (its argv, whether another agent's credential reached it), opens a
// session, runs a one-message turn per `turn/start`, and exits when stdin
// closes, unless mockMSPIgnoreEOFEnv says to ignore that (as some real hosts
// did) — then it runs until killed, with a child of its own when
// mockMSPChildPIDFileEnv names a file for that child's PID.
const (
	mockMSPDriverMode      = "msp-driver"
	mockMSPSleeperMode     = "msp-sleeper"
	mockMSPSchemaEnv       = "AIX_TEST_MSP_SCHEMA"
	mockMSPIgnoreEOFEnv    = "AIX_TEST_MSP_IGNORE_EOF"
	mockMSPChildPIDFileEnv = "AIX_TEST_MSP_CHILD_PIDFILE"
	mockMSPResumeRootEnv   = "AIX_TEST_MSP_RESUME_ROOT"
	mockMSPSessionID       = "01a1dc92-617d-7450-a3d3-88e5dcc52575"
)

func runMockMSPDriver() {
	if len(os.Args) > 1 && os.Args[1] == "schema" {
		out := ""
		for i, a := range os.Args {
			if a == "--out" && i+1 < len(os.Args) {
				out = os.Args[i+1]
			}
		}
		raw, err := os.ReadFile(os.Getenv(mockMSPSchemaEnv))
		if err != nil || out == "" || os.MkdirAll(out, 0o755) != nil ||
			os.WriteFile(filepath.Join(out, "msp.schema.json"), raw, 0o644) != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	if pidFile := os.Getenv(mockMSPChildPIDFileEnv); pidFile != "" {
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), mockCLIEnvVar+"="+mockMSPSleeperMode)
		if child.Start() == nil {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &msg) != nil || msg.Method == "" || len(msg.ID) == 0 {
			continue
		}
		answer := func(result any) {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		}
		switch msg.Method {
		case "initialize":
			answer(map[string]any{
				"serverInfo":          map[string]any{"name": "muse", "version": "1.4.20"},
				"sessionDurability":   "durable",
				"sandboxAvailability": "unavailable",
				"_meta": map[string]any{"launch": map[string]any{
					"argv":          os.Args[1:],
					"configContent": os.Getenv("ANTHROPIC_API_KEY"),
				}},
			})
		case "session/start":
			answer(map[string]any{"session": map[string]any{
				"sessionId": mockMSPSessionID, "workspaceRoot": msg.Params["workspaceRoot"],
				"approvalMode": map[string]any{"mode": msg.Params["approvalMode"]},
			}})
		case "session/resume":
			answer(map[string]any{"session": map[string]any{
				"sessionId": msg.Params["sessionId"], "workspaceRoot": os.Getenv(mockMSPResumeRootEnv),
			}})
		case "turn/start":
			turn := msg.Params["commandId"]
			answer(map[string]any{"commandId": turn, "status": "accepted", "turnId": turn, "startedNewTurn": true, "disposition": "started"})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/started", "params": map[string]any{"turnId": turn}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "item/completed", "params": map[string]any{
				"item": map[string]any{"kind": "agentMessage", "turnId": turn, "text": "hello"}}})
			now := time.Now()
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "usage/changed", "params": map[string]any{
				"window":       map[string]any{"usedPercent": 12, "windowDurationMins": 300, "resetsAtMs": now.Add(time.Hour).UnixMilli()},
				"weekly":       map[string]any{"usedPercent": 40, "resetsAtMs": now.Add(24 * time.Hour).UnixMilli()},
				"observedAtMs": now.UnixMilli(),
			}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"turnId": turn, "terminal": "completed"}})
		}
	}
	if os.Getenv(mockMSPIgnoreEOFEnv) == "1" {
		time.Sleep(10 * time.Minute)
	}
	os.Exit(0)
}

func runMockMSPSleeper() {
	time.Sleep(10 * time.Minute)
	os.Exit(0)
}

// mockMSPBinary copies the test binary into a temp dir as name.
func mockMSPBinary(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	testExe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if err := copyTestBinary(testExe, bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

// pinMSPDriverBinary makes readMSPDriverBinary answer b for one test.
func pinMSPDriverBinary(t *testing.T, b mspDriverBinary) {
	t.Helper()
	mspDriverBinaryReaderMu.Lock()
	prev := mspDriverBinaryReader
	mspDriverBinaryReader = func() mspDriverBinary { return b }
	mspDriverBinaryReaderMu.Unlock()
	t.Cleanup(func() {
		mspDriverBinaryReaderMu.Lock()
		mspDriverBinaryReader = prev
		mspDriverBinaryReaderMu.Unlock()
	})
}

func qualifyingMuse(path, version string) mspDriverBinary {
	return mspDriverBinary{path: path, found: true, version: version, surface: true}
}

// startMockMSPDriver starts Muse Code's MSP manager on the mock host.
func startMockMSPDriver(t *testing.T) (*ACPManager, string, *frameLog, string) {
	t.Helper()
	bin := mockMSPBinary(t, "muse-bin-1.4.20-R1")
	pinMSPDriverBinary(t, qualifyingMuse(bin, "1.4.20"))
	t.Setenv(mockCLIEnvVar, mockMSPDriverMode)
	m := newACPManager(nil, museCodeMSPProfile)
	log := &frameLog{}
	id := fmt.Sprintf("msp-test-%d", time.Now().UnixNano())
	dir := t.TempDir()
	if err := m.Start(id, dir, nil, "ws", "uid", ACPStartOptions{}, log.publish); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.End(id) })
	return m, id, log, dir
}

/* --- contract pins (shared-constants 3.23.580) --- */

func TestMuseCodeMSPWireValuesMatchSharedConstants(t *testing.T) {
	p := museCodeMSPProfile
	got := []string{p.cliAgentID, p.protocol,
		p.commandTypes.Start, p.commandTypes.Send, p.commandTypes.End,
		p.resultTypes.Started, p.resultTypes.Message, p.resultTypes.Stderr, p.resultTypes.Error, p.resultTypes.Ended,
		museCodeMSPMinVersion, museMSPChatApprovalMode, relayMSPInitializeRPCID, strconv.Itoa(mspDriversProtocol)}
	want := []string{"museCode", "msp",
		"musecode_msp_start", "musecode_msp_send", "musecode_msp_end",
		"musecode_msp_started", "musecode_msp_message", "musecode_msp_stderr", "musecode_msp_error", "musecode_msp_ended",
		"1.4.18", "promptUnmatched", "relay-msp-init", "1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wire values = %v, want %v", got, want)
	}
	// What terminal-service matches a refusal and an absent session on.
	if p.noun != "musecode msp" || p.startNoun != "muse serve" || !p.fenceFrames || !p.killProcessTree {
		t.Fatalf("profile = %+v", p)
	}
	if acpDriverProfileForCommand("musecode_msp_send") != p || acpDriverProfileForCommand("musecode_native_send") != nil {
		t.Fatal("the MSP commands do not route to the MSP profile")
	}
	if rejectionResultType("musecode_msp_start") != "musecode_msp_error" {
		t.Fatal("a rejected MSP start is not answered as a musecode_msp_error")
	}
	if !isSessionEndCommandType("musecode_msp_end") || !isWorkContinuationCommand(commandMsg{Type: "musecode_msp_send"}) {
		t.Fatal("the MSP END / SEND are not known as session end / work continuation")
	}
}

/* --- argv --- */

// The chat host is the probe host's opposite: no --disable-write,
// --disable-shell or --no-session-log.
func TestMuseCodeMSPArgvIsExactlyTheChatHost(t *testing.T) {
	args, err := museCodeMSPProfile.buildArgs(nil, ACPStartOptions{AllowAlwaysApprove: true, AllowAPIKeyFallback: true})
	if err != nil || strings.Join(args, " ") != "serve --disable-request-user-input" {
		t.Fatalf("argv = %v, %v", args, err)
	}
	for _, forbidden := range []string{"--disable-write", "--disable-shell", "--no-session-log"} {
		if containsString(args, forbidden) {
			t.Fatalf("the chat host runs with %s", forbidden)
		}
	}
	// The returned argv is a copy: a caller cannot edit the next session's.
	args[0] = "exec"
	if again, _ := museCodeMSPProfile.buildArgs(nil, ACPStartOptions{}); again[0] != "serve" {
		t.Fatal("buildArgs hands out the shared argv")
	}
}

func TestAMuseCodeMSPStartRefusesEveryArg(t *testing.T) {
	for _, args := range [][]string{
		{"--no-session-log"}, {"--disable-write"}, {"--disable-shell"}, {"--disable-sandbox"},
		{"--sandbox-network", "full"}, {"--listen"}, {"--enable-attach"}, {"--join"}, {"--per-session"},
		{"--provider", "echo"}, {"--model", "x"}, {"--model=x"}, {"-c", "k=v"}, {"--trust-workspace"},
		{"--yolo"}, {"anything"},
	} {
		if _, err := museCodeMSPProfile.buildArgs(args, ACPStartOptions{}); err == nil {
			t.Errorf("START args %v were accepted", args)
		}
	}
	if _, err := museCodeMSPProfile.buildArgs([]string{"--Disable-Write"}, ACPStartOptions{}); err == nil || !strings.Contains(err.Error(), "never runs with") {
		t.Errorf("a named flag is refused by name: %v", err)
	}
}

/* --- the installed Muse --- */

func TestMSPDriverEligibility(t *testing.T) {
	cases := []struct {
		name string
		b    mspDriverBinary
		want bool
	}{
		{"1.4.18 with the surface", qualifyingMuse("/m/muse-bin-1.4.18", "1.4.18"), true},
		{"1.4.20 with the surface", qualifyingMuse("/m/muse-bin-1.4.20", "1.4.20"), true},
		{"1.4.17", qualifyingMuse("/m/muse-bin-1.4.17", "1.4.17"), false},
		{"1.4.14", qualifyingMuse("/m/muse-bin-1.4.14", "1.4.14"), false},
		{"an unreadable version", qualifyingMuse("/m/muse", ""), false},
		{"1.4.20 whose schema lacks the surface", mspDriverBinary{path: "/m/muse", found: true, version: "1.4.20"}, false},
		{"a bare Windows launcher", mspDriverBinary{path: `C:\m\muse.cmd`, found: true, shim: true, version: "1.4.20", surface: true}, false},
		{"not installed", mspDriverBinary{}, false},
	}
	for _, c := range cases {
		if got := mspDriverEligible(c.b); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMuseCodeBuildVersionReadsTheBuildsOwnName(t *testing.T) {
	for name, want := range map[string]string{
		"muse-bin-1.4.20-R8619.1.exe": "1.4.20",
		"muse-bin-1.4.18-R8587.1":     "1.4.18",
		"muse.exe":                    "",
		"muse":                        "",
		"muse-bin-":                   "",
	} {
		if got := museCodeBuildVersion(filepath.Join("dir", name)); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// installMuseLauncher lays out a launcher install in a fresh PATH: the `muse`
// shim, `.muse-version`, and the versioned build when build is true.
func installMuseLauncher(t *testing.T, version string, build bool) (dir, buildPath string) {
	t.Helper()
	dir = t.TempDir()
	shim := filepath.Join(dir, "muse")
	if runtime.GOOS == "windows" {
		shim += ".cmd"
	}
	if err := os.WriteFile(shim, []byte("@echo off\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".muse-version"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buildPath = filepath.Join(dir, "muse-bin-"+version)
	if runtime.GOOS == "windows" {
		buildPath += ".exe"
	}
	if build {
		if err := os.WriteFile(buildPath, []byte("build"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir, buildPath
}

// The host runs the versioned build beside the launcher, at that build's
// own version, and only a build at the minimum has its schema read.
func TestReadInstalledMSPDriverBinaryRunsTheVersionedBuild(t *testing.T) {
	good := rawMuseMSPSchema(loadMuseMSPSurfaceFixture(t))
	var exported []string
	pinMuseMSPSchemaExport(t, func(binary string) ([]byte, error) { exported = append(exported, binary); return good, nil })

	_, build := installMuseLauncher(t, "1.4.20-R8619.1", true)
	b := readInstalledMSPDriverBinary()
	if b.path != build || !b.found || b.shim || b.version != "1.4.20" || !b.surface || !mspDriverEligible(b) {
		t.Fatalf("1.4.20 launcher install = %+v", b)
	}
	if info, _ := os.Stat(build); b.key != (versionProbeKey{Path: build, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}) {
		t.Fatalf("version identity %+v is not the build's", b.key)
	}
	if !reflect.DeepEqual(exported, []string{build}) {
		t.Fatalf("schema read from %v, want the build", exported)
	}

	exported = nil
	_, build = installMuseLauncher(t, "1.4.14-R7309.1", true)
	b = readInstalledMSPDriverBinary()
	if b.version != "1.4.14" || b.surface || mspDriverEligible(b) || len(exported) != 0 {
		t.Fatalf("1.4.14 launcher install = %+v (exported %v)", b, exported)
	}
}

// On Windows a launcher with no build beside it is a `.cmd` script: never run
// under a resident host.
func TestReadInstalledMSPDriverBinaryRefusesABareWindowsLauncher(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the .cmd launcher is Windows-only")
	}
	pinMuseMSPSchemaExport(t, func(string) ([]byte, error) { return nil, errors.New("never") })
	installMuseLauncher(t, "1.4.20-R8619.1", false)
	if b := readInstalledMSPDriverBinary(); !b.found || !b.shim || mspDriverEligible(b) {
		t.Fatalf("bare launcher = %+v", b)
	}
}

// A START re-reads the binary: anything that no longer qualifies is refused
// ACP_CLI_TOO_OLD (typed, missing Muse included, so terminal-service settles
// it from the code) and the drivers are re-read.
func TestAMuseCodeMSPStartRefusesAMuseThatNoLongerQualifies(t *testing.T) {
	scheduled := pinACPDriversRefresh(t)
	for name, b := range map[string]mspDriverBinary{
		"not installed":      {},
		"too old":            qualifyingMuse("/m/muse-bin-1.4.14", "1.4.14"),
		"no surface":         {path: "/m/muse-bin-1.4.20", found: true, version: "1.4.20"},
		"a bare launcher":    {path: `C:\m\muse.cmd`, found: true, shim: true},
		"unreadable version": {path: "/m/muse", found: true},
	} {
		pinMSPDriverBinary(t, b)
		before := *scheduled
		_, err := prepareMSPDriverBinary()
		if acpTypedErrorCode(err) != acpErrorCodeCLITooOld || *scheduled != before+1 {
			t.Errorf("%s: %v (re-reads %d)", name, err, *scheduled-before)
		}
	}
	pinMSPDriverBinary(t, qualifyingMuse("/m/muse-bin-1.4.18", "1.4.18"))
	before := *scheduled
	if b, err := prepareMSPDriverBinary(); err != nil || b.version != "1.4.18" || *scheduled != before {
		t.Fatalf("a qualifying Muse: %+v %v", b, err)
	}
}

// A refused START keeps terminal-service's settle prefix and its typed code,
// under Muse Code's own error type.
func TestMuseCodeMSPStartRefusalsKeepTheSettlePrefixAndTheirCode(t *testing.T) {
	acks := captureFollowUpAcks(t)
	pinACPDriversRefresh(t)
	prev := globalMuseCodeMSPManager
	globalMuseCodeMSPManager = newACPManager(nil, museCodeMSPProfile)
	t.Cleanup(func() { globalMuseCodeMSPManager = prev })
	dir := t.TempDir()

	pinMSPDriverBinary(t, qualifyingMuse(filepath.Join(dir, "muse"), "1.4.14"))
	handleACPCommand(context.Background(), (*pubsub.Publisher)(nil), commandMsg{ID: "c1", Type: "musecode_msp_start", SessionID: "s1", Cwd: dir, Command: "muse"}, nil)
	pinMSPDriverBinary(t, mspDriverBinary{})
	handleACPCommand(context.Background(), nil, commandMsg{ID: "c2", Type: "musecode_msp_start", SessionID: "s2", Cwd: dir}, nil)
	handleACPCommand(context.Background(), nil, commandMsg{ID: "c3", Type: "musecode_msp_start", SessionID: "s3", Cwd: dir, Args: []string{"--no-session-log"}}, nil)

	got := acks.snapshot()
	if len(got) != 3 {
		t.Fatalf("frames %+v", got)
	}
	for i, code := range []string{acpErrorCodeCLITooOld, acpErrorCodeCLITooOld, ""} {
		if got[i].Type != "musecode_msp_error" || !strings.HasPrefix(got[i].Output, "failed to start musecode msp: ") || got[i].ErrorCode != code {
			t.Errorf("refusal %d: %+v", i, got[i])
		}
	}
	if !strings.Contains(got[0].Output, "1.4.14 is older than 1.4.18") || !strings.Contains(got[1].Output, "not installed") {
		t.Errorf("refusal texts: %q / %q", got[0].Output, got[1].Output)
	}
}

/* --- the host, end to end on a stand-in `muse serve` --- */

// The whole driver: the chat host's argv, other agents' credentials stripped,
// every frame forwarded verbatim under Muse Code's own types, the host's own
// quota report kept for the CLI Agents card, a refused frame typed and never
// written, and the ended frame on END.
func TestMuseCodeMSPDriverLifecycle(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-reach-muse")
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	account := museCodeUsageAccountKey(currentMuseCodeAccountFingerprint())
	m, id, log, dir := startMockMSPDriver(t)
	if err := m.Send(id, `{"jsonrpc":"2.0","id":"relay-msp-init","method":"initialize","params":{"clientInfo":{"name":"aiexpedite","version":"1"},"capabilities":{"userInputDialogs":false}}}`); err != nil {
		t.Fatal(err)
	}
	launch := readMockLaunch(t, waitForFrame(t, log, "musecode_msp_message", `"relay-msp-init"`))
	if strings.Join(launch.Argv, " ") != "serve --disable-request-user-input" {
		t.Fatalf("argv = %v", launch.Argv)
	}
	if launch.ConfigContent != "" {
		t.Fatal("another agent's credential reached the Muse host")
	}
	if err := m.Send(id, `{"jsonrpc":"2.0","method":"initialized"}`); err != nil {
		t.Fatal(err)
	}
	start := `{"jsonrpc":"2.0","id":"relay-msp-start","method":"session/start","params":{"commandId":"01a1dc92-0000-7000-8000-000000000001","workspaceRoot":` + strconv.Quote(dir) + `,"approvalMode":"promptUnmatched"}}`
	if err := m.Send(id, start); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, log, "musecode_msp_message", `"relay-msp-start"`)
	if got := m.Get(id).fence.mspHostSession(); got != mockMSPSessionID {
		t.Fatalf("host session = %q", got)
	}
	if err := m.Send(id, `{"jsonrpc":"2.0","id":"relay-turn-t1","method":"turn/start","params":{"commandId":"01a1dc92-0000-7000-8000-000000000002","sessionId":"`+mockMSPSessionID+`","ifBusy":"queue","input":[{"type":"text","text":"hi"}]}}`); err != nil {
		t.Fatal(err)
	}
	waitForFrame(t, log, "musecode_msp_message", `"turn/completed"`)
	if reading, ok := loadMuseCodeUsageLive(account); !ok || reading.Window.UsedPercent != 12 || reading.Weekly.UsedPercent != 40 {
		t.Fatalf("the host's usage/changed was not kept: %+v ok=%v", reading, ok)
	}
	// A second conversation in the same host, and a looser mode, are refused
	// and never written.
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":"x1","method":"session/start","params":{"commandId":"01a1dc92-0000-7000-8000-000000000003","workspaceRoot":` + strconv.Quote(dir) + `,"approvalMode":"promptUnmatched"}}`,
		`{"jsonrpc":"2.0","id":"x2","method":"session/setApprovalMode","params":{"commandId":"01a1dc92-0000-7000-8000-000000000004","sessionId":"` + mockMSPSessionID + `","mode":"allowAll"}}`,
	} {
		if err := m.Send(id, frame); acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
			t.Fatalf("%s: %v", frame, err)
		}
	}
	// The identical setup again (a redelivered SEND) passes: Muse answers it
	// from its own commandId record.
	if err := m.Send(id, start); err != nil {
		t.Fatalf("a redelivered session/start: %v", err)
	}
	if err := m.End(id); err != nil {
		t.Fatal(err)
	}
	ended := waitForFrame(t, log, "musecode_msp_ended", "muse serve ended")
	if ended.ExitCode != 0 {
		t.Fatalf("ended %+v", ended)
	}
	if n := len(log.ofType("musecode_msp_error")); n != 0 {
		t.Fatalf("%d error frames on a clean session", n)
	}
	for _, f := range log.snapshot() {
		if !strings.HasPrefix(f.Type, "musecode_msp_") {
			t.Fatalf("a Muse frame went out as %s", f.Type)
		}
	}
}

// A resumed conversation that works outside the session's folder is never
// published, and the host is ended with a typed refusal.
func TestAMuseCodeMSPResumeOutsideTheFolderEndsTheHost(t *testing.T) {
	t.Setenv(mockMSPResumeRootEnv, t.TempDir())
	m, id, log, _ := startMockMSPDriver(t)
	if err := m.Send(id, `{"jsonrpc":"2.0","id":"relay-msp-resume","method":"session/resume","params":{"commandId":"01a1dc92-0000-7000-8000-000000000005","sessionId":"`+mockMSPSessionID+`","excludeItems":true}}`); err != nil {
		t.Fatal(err)
	}
	refused := waitForFrame(t, log, "musecode_msp_error", "outside the session's folder")
	if refused.ErrorCode != acpErrorCodeFrameRefused {
		t.Fatalf("refusal %+v", refused)
	}
	waitForFrame(t, log, "musecode_msp_ended", "muse serve ended")
	for _, f := range log.ofType("musecode_msp_message") {
		if strings.Contains(f.Output, "relay-msp-resume") {
			t.Fatalf("the refused answer was published: %s", f.Output)
		}
	}
}

// A host that ignores its closed stdin is killed with everything it runs,
// right after the grace period: no interrupt round first.
func TestMuseCodeMSPEndKillsTheHostTreeWhenItIgnoresEOF(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv(mockMSPIgnoreEOFEnv, "1")
	t.Setenv(mockMSPChildPIDFileEnv, pidFile)
	m, id, log, _ := startMockMSPDriver(t)
	var childPID int
	waitForLive(t, "the host's child", func() bool {
		raw, err := os.ReadFile(pidFile)
		childPID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && childPID > 0
	})
	child, err := os.FindProcess(childPID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Kill() })
	began := time.Now()
	if err := m.End(id); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > acpFamilyGracefulShutdownTimeout+3*time.Second {
		t.Fatalf("End took %v: it went through an interrupt round before killing", took)
	}
	waitForFrame(t, log, "musecode_msp_ended", "muse serve ended")
	deadline := time.Now().Add(15 * time.Second)
	for !processHandleGone(child) {
		if time.Now().After(deadline) {
			t.Fatal("the host's child outlived End")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

/* --- what the host's frames tell the device --- */

func museUsageChangedFrame(observedAt time.Time) string {
	return `{"jsonrpc":"2.0","method":"usage/changed","params":{"window":{"usedPercent":12,"windowDurationMins":300,"resetsAtMs":` +
		strconv.FormatInt(observedAt.Add(time.Hour).UnixMilli(), 10) + `},"weekly":{"usedPercent":40,"resetsAtMs":` +
		strconv.FormatInt(observedAt.Add(24*time.Hour).UnixMilli(), 10) + `},"observedAtMs":` + strconv.FormatInt(observedAt.UnixMilli(), 10) + `}}`
}

// A chat host's own `usage/changed` keeps the CLI Agents card current, filed
// under the account it was launched with — and dropped after a login switch.
func TestTheMuseCodeMSPObserverKeepsTheQuotaUnderTheLaunchAccount(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	key := museCodeUsageAccountKey(currentMuseCodeAccountFingerprint())
	observe := newMuseCodeMSPObserver("s1", key)
	at := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	observe(museUsageChangedFrame(at))
	got, ok := loadMuseCodeUsageLive(key)
	if !ok || got.ObservedAtMs != at.UnixMilli() || got.Window.UsedPercent != 12 || got.Weekly.UsedPercent != 40 {
		t.Fatalf("reading %+v ok=%v", got, ok)
	}
	// Another account signed in since the launch: nothing is filed.
	writeMuseConfig(t, home, "auth.json", strings.Replace(museAuthFixture, "Dev@Example.com", "other@example.com", 1))
	observe(museUsageChangedFrame(at.Add(30 * time.Second)))
	if again, _ := loadMuseCodeUsageLive(key); again.ObservedAtMs != at.UnixMilli() {
		t.Fatal("a reading was filed under the launch account after a login switch")
	}
}

// A turn Muse refused for sign-in marks Muse Code signed out; a completed one
// clears the mark.
func TestTheMuseCodeMSPObserverRecordsSignInFromTurns(t *testing.T) {
	isolateMuseCodeHome(t)
	observe := newMuseCodeMSPObserver("s1", museCodeUnknownAccountKey)
	observe(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"t1","terminal":"failed","error":{"kind":"authRequired","message":"run muse login"}}}`)
	if !museCodeSignedOutMarked() {
		t.Fatal("an authRequired turn did not mark Muse Code signed out")
	}
	// A quota failure is not sign-in evidence either way.
	observe(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"t2","terminal":"failed","error":{"kind":"modelError","quota":{"resetsAtMs":1}}}}`)
	if !museCodeSignedOutMarked() {
		t.Fatal("a quota failure cleared the signed-out mark")
	}
	time.Sleep(5 * time.Millisecond)
	observe(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"t3","terminal":"completed"}}`)
	if museCodeSignedOutMarked() {
		t.Fatal("a completed turn did not clear the signed-out mark")
	}
}

// A host lives for hours: after a login switch, its turns speak for the
// account it was launched with, so they neither mark the new account signed
// out nor clear a refusal of it (Codex P2 on #296).
func TestTheMuseCodeMSPObserverIgnoresSignInAfterALoginSwitch(t *testing.T) {
	home := isolateMuseCodeHome(t)
	writeMuseConfig(t, home, "auth.json", museAuthFixture)
	observe := newMuseCodeMSPObserver("s1", museCodeUsageAccountKey(currentMuseCodeAccountFingerprint()))
	writeMuseConfig(t, home, "auth.json", strings.Replace(museAuthFixture, "Dev@Example.com", "other@example.com", 1))
	observe(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"t1","terminal":"failed","error":{"kind":"authRequired"}}}`)
	if museCodeSignedOutMarked() {
		t.Fatal("the old account's refusal marked the new account signed out")
	}
	// A refusal recorded for the new account is not cleared by the old
	// account's completed turn either.
	recordMuseCodeSignedOut(time.Now(), home, os.Getenv)
	time.Sleep(5 * time.Millisecond)
	observe(`{"jsonrpc":"2.0","method":"turn/completed","params":{"turnId":"t2","terminal":"completed"}}`)
	if !museCodeSignedOutMarked() {
		t.Fatal("the old account's completed turn cleared the new account's refusal")
	}
}
