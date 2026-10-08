// claude_run_end_hook_install.go — merges our `SessionEnd` hook into Claude's
// settings.json (CLAUDE_CONFIG_DIR or ~/.claude) and removes it again.
//
// The entry we own is exactly one matcher group holding one command hook whose
// command is ours (isOurClaudeHookCommand with claudeRunEndHookArg):
//
//	"hooks": { "SessionEnd": [ { "hooks": [ { "type": "command",
//	  "command": "<ours> claude-run-end-hook", "timeout": 10 } ] } ] }
//
// Every other hook group, event and settings key is preserved: each
// round-trips as json.RawMessage, re-encoded compactly as the status-line
// installer does. Install re-points a stale binary path in place and never
// adds a second group; remove deletes only our group, then an emptied
// SessionEnd array, then an emptied hooks object. A settings.json
// that does not parse is never written — the same rule the status-line
// installer follows.
package main

import (
	"encoding/json"
	"fmt"
)

// claudeRunEndHookEvent is the Claude Code hook event we register on: it fires
// once per session in both `--print` and interactive mode (Stop would fire
// after every interactive turn).
const claudeRunEndHookEvent = "SessionEnd"

// claudeRunEndHookTimeoutSeconds bounds how long Claude waits on our hook. The
// owe is one bounded cache write; the cap only matters on a wedged filesystem.
const claudeRunEndHookTimeoutSeconds = 10

// claudeHookCommandEntry mirrors one command hook inside a matcher group.
type claudeHookCommandEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// claudeHookMatcherGroup mirrors one matcher group of a hook event.
type claudeHookMatcherGroup struct {
	Matcher string                   `json:"matcher,omitempty"`
	Hooks   []claudeHookCommandEntry `json:"hooks"`
}

// isOurRunEndHookGroup reports whether a raw matcher group is the one we own:
// a single command hook whose command is our run-end hook.
func isOurRunEndHookGroup(raw json.RawMessage) bool {
	var g claudeHookMatcherGroup
	if json.Unmarshal(raw, &g) != nil || len(g.Hooks) != 1 {
		return false
	}
	h := g.Hooks[0]
	return h.Type == "command" && isOurClaudeHookCommand(h.Command, claudeRunEndHookArg)
}

// ensureClaudeRunEndHook installs or re-points our SessionEnd hook. Returns
// true when it wrote a change.
func ensureClaudeRunEndHook(home string) (bool, error) {
	settingsPath := claudeSettingsPathIfPresent(home)
	if settingsPath == "" {
		return false, nil
	}
	ours, ok := ourClaudeHookCommand(claudeRunEndHookArg)
	if !ok {
		return false, nil
	}
	settings, _, err := readClaudeSettings(settingsPath)
	if err != nil {
		return false, err
	}
	hooks, groups, err := decodeClaudeHookEvent(settings, claudeRunEndHookEvent)
	if err != nil {
		return false, err
	}
	want, err := json.Marshal(claudeHookMatcherGroup{Hooks: []claudeHookCommandEntry{{
		Type: "command", Command: ours, Timeout: claudeRunEndHookTimeoutSeconds,
	}}})
	if err != nil {
		return false, err
	}

	// Keep the first group of ours (re-pointed), drop any duplicate, and leave
	// every other group where it was.
	out := make([]json.RawMessage, 0, len(groups)+1)
	found, changed := false, false
	for _, g := range groups {
		if !isOurRunEndHookGroup(g) {
			out = append(out, g)
			continue
		}
		if found {
			changed = true
			continue
		}
		found = true
		var cur claudeHookMatcherGroup
		_ = json.Unmarshal(g, &cur)
		if cur.Matcher == "" && cur.Hooks[0].Command == ours && cur.Hooks[0].Timeout == claudeRunEndHookTimeoutSeconds {
			out = append(out, g)
			continue
		}
		out = append(out, want)
		changed = true
	}
	if !found {
		out = append(out, want)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if err := encodeClaudeHookEvent(settings, hooks, claudeRunEndHookEvent, out); err != nil {
		return false, err
	}
	return writeClaudeSettings(settingsPath, settings)
}

// removeClaudeRunEndHook deletes our SessionEnd group, pruning containers it
// leaves empty. Returns true when it wrote a change.
func removeClaudeRunEndHook(home string) (bool, error) {
	settingsPath := claudeSettingsPathIfPresent(home)
	if settingsPath == "" {
		return false, nil
	}
	settings, exists, err := readClaudeSettings(settingsPath)
	if err != nil || !exists {
		return false, err
	}
	hooks, groups, err := decodeClaudeHookEvent(settings, claudeRunEndHookEvent)
	if err != nil {
		return false, err
	}
	out := make([]json.RawMessage, 0, len(groups))
	for _, g := range groups {
		if !isOurRunEndHookGroup(g) {
			out = append(out, g)
		}
	}
	if len(out) == len(groups) {
		return false, nil
	}
	if err := encodeClaudeHookEvent(settings, hooks, claudeRunEndHookEvent, out); err != nil {
		return false, err
	}
	return writeClaudeSettings(settingsPath, settings)
}

// decodeClaudeHookEvent returns settings' `hooks` object (empty when absent)
// and the matcher groups of `event` (nil when absent). A `hooks` or event value
// of the wrong JSON type is an error, so a hand-edited shape we do not
// understand is left untouched.
func decodeClaudeHookEvent(settings map[string]json.RawMessage, event string) (map[string]json.RawMessage, []json.RawMessage, error) {
	hooks := map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, fmt.Errorf("claude settings: hooks is not an object: %w", err)
		}
		if hooks == nil {
			hooks = map[string]json.RawMessage{}
		}
	}
	var groups []json.RawMessage
	if raw, ok := hooks[event]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return nil, nil, fmt.Errorf("claude settings: hooks.%s is not an array: %w", event, err)
		}
	}
	return hooks, groups, nil
}

// encodeClaudeHookEvent writes `groups` back as hooks[event], deleting the
// event when it is empty and the hooks object when that leaves it empty.
func encodeClaudeHookEvent(settings, hooks map[string]json.RawMessage, event string, groups []json.RawMessage) error {
	if len(groups) == 0 {
		delete(hooks, event)
	} else {
		raw, err := json.Marshal(groups)
		if err != nil {
			return err
		}
		hooks[event] = raw
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
		return nil
	}
	raw, err := json.Marshal(hooks)
	if err != nil {
		return err
	}
	settings["hooks"] = raw
	return nil
}

// applyClaudeRunEndHook installs the run-end hook when `wanted`, removes it
// otherwise. `wanted` is "neither opt-out is on": a debt the probe can never
// pay would be pure noise, and "don't touch my Claude settings" must not be
// half honoured.
func applyClaudeRunEndHook(home string, wanted bool) (bool, error) {
	if wanted {
		return ensureClaudeRunEndHook(home)
	}
	return removeClaudeRunEndHook(home)
}
