package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The device's client allowlist is shared-constants
// ACP_CLIENT_METHOD_ALLOWLIST.opencodeAcp / .antigravityAcp (identical).
func TestACPFenceAllowlistMatchesSharedConstants(t *testing.T) {
	want := []string{"initialize", "session/new", "session/load", "session/resume", "session/prompt", "session/set_config_option", "session/close"}
	if strings.Join(acpFamilyClientRequests, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v", acpFamilyClientRequests)
	}
	if strings.Join(acpFamilyClientNotifications, ",") != "session/cancel" {
		t.Errorf("notifications = %v", acpFamilyClientNotifications)
	}
	if strings.Join(acpFamilyClientConfigOptionIDs, ",") != "model,effort,thought_level" {
		t.Errorf("config option ids = %v", acpFamilyClientConfigOptionIDs)
	}
}

func newTestFence(t *testing.T) (*acpClientFence, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newACPClientFence(openCodeACPProfile), root
}

func wantRefused(t *testing.T, f *acpClientFence, root, frame string) {
	t.Helper()
	_, err := f.check(frame, root)
	if acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("frame %s: %v, want %s", frame, err, acpErrorCodeFrameRefused)
	}
}

func wantAllowed(t *testing.T, f *acpClientFence, root, frame string) acpFenceCheck {
	t.Helper()
	c, err := f.check(frame, root)
	if err != nil {
		t.Fatalf("frame %s refused: %v", frame, err)
	}
	return c
}

func TestACPFenceAllowsWhatTerminalServiceSends(t *testing.T) {
	f, root := newTestFence(t)
	cwd := strings.ReplaceAll(root, `\`, `\\`)
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":"relay-acp-init","method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}}}`,
		`{"jsonrpc":"2.0","id":"relay-acp-new","method":"session/new","params":{"cwd":"` + cwd + `","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":"relay-acp-resume","method":"session/resume","params":{"sessionId":"ses_1","cwd":"` + cwd + `","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":"relay-acp-load","method":"session/load","params":{"sessionId":"ses_1","cwd":"` + cwd + `","mcpServers":[]}}`,
		`{"jsonrpc":"2.0","id":"relay-acp-config-model","method":"session/set_config_option","params":{"sessionId":"ses_1","configId":"model","value":"opencode/big-pickle"}}`,
		`{"jsonrpc":"2.0","id":"relay-acp-config-effort","method":"session/set_config_option","params":{"sessionId":"ses_1","configId":"effort","value":"high"}}`,
		`{"jsonrpc":"2.0","id":"relay-acp-config-effort","method":"session/set_config_option","params":{"sessionId":"ses_1","configId":"thought_level","value":"low"}}`,
		`{"jsonrpc":"2.0","id":"acp-turn-t1","method":"session/prompt","params":{"sessionId":"ses_1","prompt":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","id":"relay-turn-fu_a1","method":"session/prompt","params":{"sessionId":"ses_1","prompt":[{"type":"text","text":"hi"}]}}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"ses_1"}}`,
		`{"jsonrpc":"2.0","id":9,"method":"session/close","params":{"sessionId":"ses_1"}}`,
	} {
		wantAllowed(t, f, root, frame)
	}
}

func TestACPFenceRefusesWhatWouldWidenOrSilenceTheSession(t *testing.T) {
	f, root := newTestFence(t)
	cwd := strings.ReplaceAll(root, `\`, `\\`)
	outside := strings.ReplaceAll(filepath.Dir(root), `\`, `\\`)
	for name, frame := range map[string]string{
		"fork":                  `{"jsonrpc":"2.0","id":1,"method":"session/fork","params":{"sessionId":"s"}}`,
		"delete":                `{"jsonrpc":"2.0","id":1,"method":"session/delete","params":{"sessionId":"s"}}`,
		"set_mode":              `{"jsonrpc":"2.0","id":1,"method":"session/set_mode","params":{"sessionId":"s","modeId":"auto"}}`,
		"mode config option":    `{"jsonrpc":"2.0","id":1,"method":"session/set_config_option","params":{"sessionId":"s","configId":"mode","value":"accept-edits"}}`,
		"config option missing": `{"jsonrpc":"2.0","id":1,"method":"session/set_config_option","params":{"sessionId":"s","value":"x"}}`,
		"list":                  `{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`,
		"authenticate":          `{"jsonrpc":"2.0","id":1,"method":"authenticate","params":{"methodId":"opencode-login"}}`,
		"steering from a SEND":  `{"jsonrpc":"2.0","id":"relay-followup-x","method":"_session/steering","params":{"sessionId":"s","prompt":[]}}`,
		"unknown extension":     `{"jsonrpc":"2.0","id":1,"method":"_x.ai/interject","params":{}}`,
		"cancel as a request":   `{"jsonrpc":"2.0","id":1,"method":"session/cancel","params":{"sessionId":"s"}}`,
		"prompt as notify":      `{"jsonrpc":"2.0","method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`,
		"additionalDirectories": `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"` + cwd + `","mcpServers":[],"additionalDirectories":["/etc"]}}`,
		"additionalDirs object": `{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"s","cwd":"` + cwd + `","additionalDirectories":{"a":"/etc"}}}`,
		"mcpServers":            `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"` + cwd + `","mcpServers":[{"name":"x","command":"sh","args":[]}]}}`,
		"resume outside":        `{"jsonrpc":"2.0","id":1,"method":"session/resume","params":{"sessionId":"s","cwd":"` + outside + `"}}`,
		"new outside":           `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"` + outside + `","mcpServers":[]}}`,
		"load relative":         `{"jsonrpc":"2.0","id":1,"method":"session/load","params":{"sessionId":"s","cwd":"sub"}}`,
		"params not an object":  `{"jsonrpc":"2.0","id":1,"method":"session/new","params":"x"}`,
		"neither":               `{"jsonrpc":"2.0","id":1}`,
		"answer to nothing":     `{"jsonrpc":"2.0","id":7,"result":{}}`,
	} {
		t.Run(name, func(t *testing.T) { wantRefused(t, f, root, frame) })
	}
}

// A subfolder of the session's own folder is inside it.
func TestACPFenceAllowsASubfolderOfTheSessionFolder(t *testing.T) {
	f, root := newTestFence(t)
	sub := filepath.Join(root, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wantAllowed(t, f, root, `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"`+strings.ReplaceAll(sub, `\`, `\\`)+`","mcpServers":[]}}`)
}

// An answer passes only for a request the CLI asked and nobody answered yet,
// and a permission answer only with a one-time option (W-O9 / W-O21: OpenCode
// asks with a NUMBER id and offers once / always / reject).
func TestACPFenceAdmitsOnlyAnswersToOpenRequests(t *testing.T) {
	f, root := newTestFence(t)
	f.inbound(`{"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"s","toolCall":{"title":"echo"},"options":[{"optionId":"once","kind":"allow_once"},{"optionId":"always","kind":"allow_always"},{"optionId":"reject","kind":"reject_once"}]}}`)
	f.inbound(`{"jsonrpc":"2.0","id":"q-1","method":"fs/read_text_file","params":{}}`)

	wantRefused(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"always"}}}`)
	wantRefused(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"nope"}}}`)
	wantRefused(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"approved"}}}`)
	wantRefused(t, f, root, `{"jsonrpc":"2.0","id":"0","result":{"outcome":{"outcome":"cancelled"}}}`)

	c := wantAllowed(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"once"}}}`)
	wantAllowed(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"reject"}}}`)
	wantAllowed(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"cancelled"}}}`)
	wantAllowed(t, f, root, `{"jsonrpc":"2.0","id":"q-1","result":{}}`)

	// Answered once: a second answer is refused.
	f.written(c)
	wantRefused(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"selected","optionId":"once"}}}`)
}

func TestACPFenceKeepsItsOpenRequestsBounded(t *testing.T) {
	f, root := newTestFence(t)
	for i := 0; i < acpFenceOpenRequestsMax+40; i++ {
		f.inbound(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i) + `,"method":"session/request_permission","params":{"options":[{"optionId":"once","kind":"allow_once"}]}}`)
	}
	if len(f.open) != acpFenceOpenRequestsMax {
		t.Fatalf("%d open requests kept", len(f.open))
	}
	// The oldest went first; the newest is still answerable.
	wantRefused(t, f, root, `{"jsonrpc":"2.0","id":0,"result":{"outcome":{"outcome":"cancelled"}}}`)
	wantAllowed(t, f, root, `{"jsonrpc":"2.0","id":`+strconv.Itoa(acpFenceOpenRequestsMax+39)+`,"result":{"outcome":{"outcome":"cancelled"}}}`)
}

// The fence is wired into Send for the selected drivers, and not for Grok,
// whose frames keep their setup-cwd gate alone.
func TestACPSendFencesOnlyTheSelectedDrivers(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child := newFakeJSONRPCChild(t)
	t.Cleanup(func() { child.exit(); _ = child.stdinW.Close() })

	m := newACPManager(nil, openCodeACPProfile)
	m.sessions["oc"] = &ACPSession{ID: "oc", Stdin: child.stdinW, status: "running", WorkspaceRoot: root, fence: newACPClientFence(openCodeACPProfile)}
	err = m.Send("oc", `{"jsonrpc":"2.0","id":1,"method":"session/set_mode","params":{"sessionId":"s","modeId":"auto"}}`)
	if acpTypedErrorCode(err) != acpErrorCodeFrameRefused {
		t.Fatalf("opencode set_mode: %v", err)
	}

	g := NewGrokACPManager(nil)
	g.sessions["gk"] = &GrokACPSession{ID: "gk", Stdin: child.stdinW, status: "running", WorkspaceRoot: root}
	if err := g.Send("gk", `{"jsonrpc":"2.0","id":2,"method":"_x.ai/interject","params":{"sessionId":"s","text":"x","interjectionId":"a"}}`); err != nil {
		t.Fatalf("a Grok frame was fenced by the family allowlist: %v", err)
	}
	waitForLive(t, "the Grok frame", func() bool { return len(child.framesWithMethod("_x.ai/interject")) == 1 })
	if len(child.framesWithMethod("session/set_mode")) != 0 {
		t.Fatal("a refused frame reached the CLI")
	}
}
