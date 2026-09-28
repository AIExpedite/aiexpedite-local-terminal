package main

import (
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
	museFrameCompleted  = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":27,"record_type":"event","payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","terminal":"completed","text":"Hello world","reason":null}}`
	museFrameUserInput  = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":3,"record_type":"status","payload_type":"turn.input.user","payload":{"kind":"turn_input_user","prompt":"say hello"}}`
	museFrameScheduled  = `{"schema_version":1,"stream":{"kind":"session","id":"0b0e7c4e-6a53-4f0e-9d0a-2f6c1f7c9a11"},"sequence":8,"record_type":"event","payload_type":"task.lifecycle.scheduled","payload":{"kind":"task_lifecycle","task_id":"01a0e091-4b4c-73f3-919b-83ffbfda4355"}}`
	museFrameFailed402  = `{"schema_version":1,"sequence":9,"record_type":"event","payload_type":"run.terminal.failed","payload":{"kind":"run_terminal","terminal":"failed","error_kind":"billing_not_configured","status":402,"reason":"Billing is not configured for this account."}}`
)

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
