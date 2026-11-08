package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Stub `muse` binary for the Muse Code / one-shot core tests.
//
// CI has no real Muse Code binary. The compiled, env-driven stub from
// opencode_stub_test.go is CLI-agnostic (it prints recorded stdout/stderr,
// exits with a chosen code, logs argv and stdin, sleeps on demand), so it is
// installed here under the name `muse` rather than compiling a second copy.
// Its env contract is documented there (OPENCODE_STUB_*); the helpers below
// name the Muse Code uses.
//
// Coverage gap, by construction: the recorded frames are the documented
// `exec --json` envelope, so an upstream change to event names or resume
// semantics is caught at runtime (version floor, fail-closed parsing), not here.

const museCodeStubVersion = "muse 1.4.0 (1.4.0-R4161.1)"

// installMuseCodeStub puts the stub first on PATH as `muse` and returns a
// fresh manager whose capability cache has not been populated.
func installMuseCodeStub(t *testing.T) *oneShotNativeManager {
	t.Helper()
	stub := buildOpenCodeStub(t)
	binDir := t.TempDir()
	name := "muse"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	data, err := os.ReadFile(stub)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, name), data, 0o755); err != nil {
		t.Fatalf("install stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OPENCODE_STUB_VERSION", museCodeStubVersion)
	return NewMuseCodeNativeManager()
}

// museStubStdout joins recorded JSONL frames in the stub's \n-escaped form.
func museStubStdout(lines ...string) string {
	return strings.Join(lines, `\n`) + `\n`
}

// Recorded `muse exec --json` frames, trimmed from a Muse Code 1.4.0
// (1.4.0-R4302.1) `--provider echo` capture (delta / completed / task
// lifecycle). The failed frame's error_kind + status are the third-party
// reported shape, not yet captured here.
const (
	museFrameDeltaHello = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":17,"record_type":"status","payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":"Hello"}}`
	museFrameDeltaWorld = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":18,"record_type":"status","payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":" world"}}`
	museFrameTool       = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":19,"record_type":"event","payload_type":"task.lifecycle.started","payload":{"kind":"task_lifecycle","task_id":"01a0e091-4b4c-73f3-919b-83ffbfda4355"}}`
	// Shape of a Muse Code 1.4.2 `tool.result` (captured from a real run).
	museFrameToolResult = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":29,"record_type":"event","payload_type":"tool.result","payload":{"kind":"tool_result","call_id":"call_01a10788df5d73e0a5e2422fc0b0eb7d","text":"{\"command\":\"echo hello\",\"exit_code\":0}","correlation_facts":{"tool_name":"powershell","outcome":"success"}}}`
	museFrameCompleted  = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":27,"record_type":"event","payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","terminal":"completed","text":"Hello world","reason":null}}`
	museFrameUserInput  = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":3,"record_type":"status","payload_type":"turn.input.user","payload":{"kind":"turn_input_user","prompt":"say hello"}}`
	museFrameScheduled  = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":8,"record_type":"event","payload_type":"task.lifecycle.scheduled","payload":{"kind":"task_lifecycle","task_id":"01a0e091-4b4c-73f3-919b-83ffbfda4355"}}`
	museFrameFailed402  = `{"schema_version":1,"sequence":9,"record_type":"event","payload_type":"run.terminal.failed","payload":{"kind":"run_terminal","terminal":"failed","error_kind":"billing_not_configured","status":402,"reason":"Billing is not configured for this account."}}`
)

// museRecord renders one `muse exec --json` record in the CLI's own field
// order (payload_type ahead of payload, as in the captured frames above), so a
// redaction that masks a credential inside the payload text never takes the
// payload_type with it. payload is the raw JSON object of the payload.
func museRecord(payloadType, payload string) string {
	return `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"record_type":"event","payload_type":"` +
		payloadType + `","payload":` + payload + `}`
}

// museDeltaRecord is a `run.output.delta` record streaming text. The text
// must hold no line break: the stub unescapes `\n` inside its stdout.
func museDeltaRecord(text string) string {
	return museRecord("run.output.delta", `{"kind":"run_output_delta","text":`+museJSONString(text)+`}`)
}

// museCompletedRecord is the `run.terminal.completed` record carrying the
// run's full final text.
func museCompletedRecord(text string) string {
	return museRecord("run.terminal.completed", `{"kind":"run_terminal","terminal":"completed","text":`+museJSONString(text)+`,"reason":null}`)
}

func museJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// museFrameOrder names each published frame in order. A message frame is
// "delta" (the device's merged run.output.delta, recognised by its leading
// kind, which precedes the text and so survives a mask that eats the rest of
// the line), "terminal" (a run.terminal.* record), "complete" (the
// aiexpedite.turn_complete envelope) or "other"; an error frame is "error"
// and a stderr frame "stderr".
func museFrameOrder(frames []resultMsg) []string {
	order := make([]string, 0, len(frames))
	for _, f := range frames {
		switch {
		case f.Type == "musecode_native_error":
			order = append(order, "error")
		case f.Type == "musecode_native_stderr":
			order = append(order, "stderr")
		case f.Type != "musecode_native_message":
			order = append(order, f.Type)
		case strings.Contains(f.Output, `"type":"aiexpedite.turn_complete"`):
			order = append(order, "complete")
		case strings.Contains(f.Output, `"payload_type":"run.terminal.`):
			order = append(order, "terminal")
		case strings.HasPrefix(f.Output, `{"payload":{"kind":"run_output_delta"`):
			order = append(order, "delta")
		default:
			order = append(order, "other")
		}
	}
	return order
}

// museFramesDump lists published frames for a failure message.
func museFramesDump(frames []resultMsg) string {
	var b strings.Builder
	for i, f := range frames {
		fmt.Fprintf(&b, "  #%d %s: %s\n", i, f.Type, f.Output)
	}
	return b.String()
}

// museStreamedDeltaText joins the text of every merged delta frame, in order.
func museStreamedDeltaText(t *testing.T, frames []resultMsg) string {
	t.Helper()
	var b strings.Builder
	for _, f := range frames {
		if !strings.HasPrefix(f.Output, `{"payload":{"kind":"run_output_delta"`) {
			continue
		}
		var rec struct {
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(f.Output), &rec); err != nil {
			t.Fatalf("a delta frame is not a JSON record: %v: %s", err, f.Output)
		}
		b.WriteString(rec.Payload.Text)
	}
	return b.String()
}

// frameSink records published frames for assertions.
type frameSink struct {
	mu     sync.Mutex
	frames []resultMsg
}

func (s *frameSink) publish(msg resultMsg) {
	s.mu.Lock()
	s.frames = append(s.frames, msg)
	s.mu.Unlock()
}

// all returns every published frame, in publish order.
func (s *frameSink) all() []resultMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]resultMsg(nil), s.frames...)
}

func (s *frameSink) ofType(t string) []resultMsg {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []resultMsg
	for _, f := range s.frames {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

// completion returns the last aiexpedite.turn_complete frame, if any.
func (s *frameSink) completion() (resultMsg, bool) {
	msgs := s.ofType("musecode_native_message")
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.Contains(msgs[i].Output, `"aiexpedite.turn_complete"`) {
			return msgs[i], true
		}
	}
	return resultMsg{}, false
}
