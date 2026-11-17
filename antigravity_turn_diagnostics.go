// File: antigravity_turn_diagnostics.go
// -----------------------------------------------------------------------------
// Keeps the evidence of a failed one-shot Antigravity turn on the person's
// computer, after later agy runs (usage probes included) have replaced
// cli.log: one agent.log line per failed turn, and one bounded JSON file per
// failed turn under <configDir>/logs/antigravity-turns/.
//
// Everything kept excludes prompts, assistant responses, credentials, the init
// tool list and tool arguments: the file holds the run's failure lines, the
// redacted stderr and a redacted tail of stdout events reduced to their event
// type, step type, state and tool name (antigravityDiagnosticEvent). agy's own
// per-run log (log/cli-<stamp>.log) already survives later runs; the file
// records which one holds this run's block.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	antigravityDiagnosticsKeep      = 20
	antigravityDiagnosticsMaxBytes  = 64 * 1024
	antigravityDiagnosticsTailLines = 20
)

// antigravityTurnDiagnosticsDir is a test seam.
var antigravityTurnDiagnosticsDir = func() string {
	return filepath.Join(GetConfigDir(), "logs", "antigravity-turns")
}

type antigravityDiagnosticEvent struct {
	Event     string `json:"event"`
	StepType  string `json:"stepType,omitempty"`
	State     string `json:"state,omitempty"`
	ToolName  string `json:"toolName,omitempty"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
	PlainText string `json:"plainText,omitempty"`
}

type antigravityTurnDiagnostics struct {
	SessionID    string                       `json:"sessionId"`
	At           string                       `json:"at"`
	AgyVersion   string                       `json:"agyVersion,omitempty"`
	ExitCode     int                          `json:"exitCode"`
	Category     string                       `json:"category,omitempty"`
	Source       string                       `json:"source"`
	Argv         []string                     `json:"argv"`
	Cwd          string                       `json:"cwd"`
	PID          int                          `json:"pid,omitempty"`
	AgyLog       string                       `json:"agyLog,omitempty"`
	FailureLines []string                     `json:"failureLines,omitempty"`
	Stderr       string                       `json:"stderr,omitempty"`
	StdoutTail   []antigravityDiagnosticEvent `json:"stdoutTail,omitempty"`
}

// antigravityDiagnosticEvents reduces stdout lines to their redacted shape,
// last antigravityDiagnosticsTailLines kept. The init event (tool list),
// user events (the prompt) and assistant text are dropped entirely.
func antigravityDiagnosticEvents(lines []string) []antigravityDiagnosticEvent {
	var out []antigravityDiagnosticEvent
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			out = append(out, antigravityDiagnosticEvent{Event: "text", PlainText: truncateRunes(redactAgentSecrets(line), 400)})
			continue
		}
		var event antigravityStreamEvent
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		switch event.Event {
		case "init", "user", "":
			continue
		case "result":
			d := antigravityDiagnosticEvent{Event: "result"}
			if event.Result != nil {
				d.Status = event.Result.Status
				if strings.EqualFold(event.Result.Status, "ERROR") {
					d.Error = truncateRunes(redactAgentSecrets(event.Result.Error), 400)
				}
			}
			out = append(out, d)
		case "step_update":
			d := antigravityDiagnosticEvent{Event: "step_update"}
			if s := event.StepUpdate; s != nil {
				d.StepType, d.State, d.ToolName = s.StepType, s.State, s.ToolName
				if msg := s.failureSentence(); msg != "" {
					d.Error = truncateRunes(redactAgentSecrets(msg), 400)
				}
			}
			out = append(out, d)
		default:
			out = append(out, antigravityDiagnosticEvent{Event: event.Event})
		}
	}
	if len(out) > antigravityDiagnosticsTailLines {
		out = out[len(out)-antigravityDiagnosticsTailLines:]
	}
	return out
}

// antigravityRedactedArgv is the turn's argv with the conversation id elided
// (the prompt never rides argv).
func antigravityRedactedArgv(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--conversation" {
			out[i+1] = "<id>"
		}
	}
	return out
}

// recordAntigravityTurnFailure writes the agent.log line and the diagnostics
// file for one failed turn. category is "" for the shutting-down failure.
// Never fails the turn: a write error is logged and dropped.
func recordAntigravityTurnFailure(d antigravityTurnDiagnostics) {
	cat := d.Category
	if cat == "" {
		cat = "none"
	}
	ver := d.AgyVersion
	if ver == "" {
		ver = "unknown"
	}
	fmt.Printf("%s[antigravity-native] Turn failed on %s: exit=%d category=%s source=%s agy=%s%s\n",
		colorYellow, d.SessionID, d.ExitCode, cat, d.Source, ver, colorReset)

	dir := antigravityTurnDiagnosticsDir()
	if dir == "" {
		return
	}
	d.Stderr = truncateRunes(redactAgentSecrets(d.Stderr), 16*1024)
	for i, l := range d.FailureLines {
		d.FailureLines[i] = truncateRunes(redactAgentSecrets(l), 2000)
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	for err == nil && len(raw) > antigravityDiagnosticsMaxBytes && (len(d.StdoutTail) > 0 || d.Stderr != "") {
		if len(d.StdoutTail) > 0 {
			d.StdoutTail = d.StdoutTail[1:]
		} else {
			d.Stderr = truncateRunes(d.Stderr, len(d.Stderr)/2)
		}
		raw, err = json.MarshalIndent(d, "", "  ")
	}
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Printf("%s[antigravity-native] Could not keep turn diagnostics: %v%s\n", colorYellow, err, colorReset)
		return
	}
	name := fmt.Sprintf("%s-%d.json", sanitizeDiagnosticsName(d.SessionID), time.Now().UnixMilli())
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		fmt.Printf("%s[antigravity-native] Could not keep turn diagnostics: %v%s\n", colorYellow, err, colorReset)
		return
	}
	pruneAntigravityTurnDiagnostics(dir)
}

func sanitizeDiagnosticsName(id string) string {
	var b strings.Builder
	for _, r := range id {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "session"
	}
	return truncateRunes(b.String(), 80)
}

// pruneAntigravityTurnDiagnostics keeps the newest antigravityDiagnosticsKeep
// files.
func pruneAntigravityTurnDiagnostics(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{e.Name(), info.ModTime()})
	}
	if len(files) <= antigravityDiagnosticsKeep {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if !files[i].mod.Equal(files[j].mod) {
			return files[i].mod.After(files[j].mod)
		}
		return files[i].name > files[j].name
	})
	for _, f := range files[antigravityDiagnosticsKeep:] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}
