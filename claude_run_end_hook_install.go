// claude_run_end_hook_install.go — merges our `SessionEnd` hook into Claude's
// settings.json (CLAUDE_CONFIG_DIR or ~/.claude) and removes it again.
//
// The entry we own is one command hook whose command is ours
// (isOurClaudeHookCommand with claudeRunEndHookArg), installed as its own
// matcher group:
//
//	"hooks": { "SessionEnd": [ { "hooks": [ { "type": "command",
//	  "command": "<ours> claude-run-end-hook", "timeout": 10 } ] } ] }
//
// Ownership is judged per command hook, not per group: a user or another tool
// may add a sibling command to the group holding ours, and that group is
// still where our hook lives. Every other command hook, group, event and
// settings key is preserved: each round-trips as json.RawMessage, re-encoded
// compactly as the status-line installer does. Install re-points a stale
// binary path in place (one copy, in a group without a matcher, so it fires on
// every session end) and never adds a second copy; remove deletes only our
// command hook, then a group it leaves empty, then an emptied SessionEnd
// array, then an emptied hooks object. A settings.json that does not parse is
// never written — the same rule the status-line installer follows.
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

// ourRunEndHookEntry decodes one command hook and reports whether it is ours.
func ourRunEndHookEntry(raw json.RawMessage) (claudeHookCommandEntry, bool) {
	var h claudeHookCommandEntry
	if json.Unmarshal(raw, &h) != nil {
		return h, false
	}
	return h, h.Type == "command" && isOurClaudeHookCommand(h.Command, claudeRunEndHookArg)
}

// rewriteOurRunEndHooks walks every command hook of every SessionEnd group.
// With want == nil it drops each of ours. Otherwise it keeps the FIRST of ours
// that sits in a group without a matcher — re-pointed to want unless it
// already matches `ours` and the timeout — and drops every other copy.
// Sibling hooks keep their place, a group left with no hooks is dropped, and a
// group whose shape we do not understand is passed through untouched. Reports
// whether a copy was kept and whether anything changed.
func rewriteOurRunEndHooks(groups []json.RawMessage, want json.RawMessage, ours string) (out []json.RawMessage, kept, changed bool, err error) {
	out = make([]json.RawMessage, 0, len(groups)+1)
	for _, g := range groups {
		var group map[string]json.RawMessage
		var entries []json.RawMessage
		if json.Unmarshal(g, &group) != nil || json.Unmarshal(group["hooks"], &entries) != nil {
			out = append(out, g)
			continue
		}
		var matcher string
		if raw, ok := group["matcher"]; ok {
			_ = json.Unmarshal(raw, &matcher)
		}
		next := make([]json.RawMessage, 0, len(entries))
		groupChanged := false
		for _, e := range entries {
			cur, mine := ourRunEndHookEntry(e)
			switch {
			case !mine:
				next = append(next, e)
			case want == nil || kept || matcher != "":
				groupChanged = true
			default:
				kept = true
				if cur.Command == ours && cur.Timeout == claudeRunEndHookTimeoutSeconds {
					next = append(next, e)
				} else {
					next = append(next, want)
					groupChanged = true
				}
			}
		}
		if !groupChanged {
			out = append(out, g)
			continue
		}
		changed = true
		if len(next) == 0 {
			continue
		}
		if group["hooks"], err = json.Marshal(next); err != nil {
			return nil, false, false, err
		}
		raw, err := json.Marshal(group)
		if err != nil {
			return nil, false, false, err
		}
		out = append(out, raw)
	}
	return out, kept, changed, nil
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
	entry := claudeHookCommandEntry{Type: "command", Command: ours, Timeout: claudeRunEndHookTimeoutSeconds}
	wantEntry, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}

	// Keep the first copy of ours (re-pointed), drop any duplicate, and leave
	// every other hook where it was.
	out, kept, changed, err := rewriteOurRunEndHooks(groups, wantEntry, ours)
	if err != nil {
		return false, err
	}
	if !kept {
		want, err := json.Marshal(claudeHookMatcherGroup{Hooks: []claudeHookCommandEntry{entry}})
		if err != nil {
			return false, err
		}
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

// removeClaudeRunEndHook deletes our SessionEnd command hook, pruning containers
// it leaves empty. Returns true when it wrote a change.
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
	out, _, changed, err := rewriteOurRunEndHooks(groups, nil, "")
	if err != nil || !changed {
		return false, err
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

// installedClaudeRunEndCachePath returns the cache path our INSTALLED SessionEnd
// hook pins, or "" when settings.json holds no run-end hook of ours. It is read
// from the hook itself, not inferred from the status line: a partial
// settings.json rewrite can drop `statusLine` and leave this hook pinned to
// another channel's cache, which is then where its debts land.
func installedClaudeRunEndCachePath(home string) string {
	settingsPath := claudeSettingsPathIfPresent(home)
	if settingsPath == "" {
		return ""
	}
	settings, exists, err := readClaudeSettings(settingsPath)
	if err != nil || !exists {
		return ""
	}
	_, groups, err := decodeClaudeHookEvent(settings, claudeRunEndHookEvent)
	if err != nil {
		return ""
	}
	for _, g := range groups {
		var group struct {
			Hooks []json.RawMessage `json:"hooks"`
		}
		if json.Unmarshal(g, &group) != nil {
			continue
		}
		for _, e := range group.Hooks {
			if h, mine := ourRunEndHookEntry(e); mine {
				if pinned := extractInstalledPinnedPath(h.Command, "RL_CACHE"); pinned != "" {
					return pinned
				}
			}
		}
	}
	return ""
}
