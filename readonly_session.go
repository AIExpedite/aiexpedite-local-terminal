// readonly_session.go — enforced read-only coding-agent sessions.
//
// A nightly Security review runs a coding agent over UNTRUSTED repository
// content. "Read-only" therefore has to be a property this device enforces,
// never a request a prompt — or the repository under review — can talk its way
// past:
//
//   - The wire field `readOnly` is SIGNED (signaturePayload.ReadOnly), so it
//     can be neither stripped nor injected in transit.
//   - Only the generic session_start honours it, and only for the BARE
//     command `claude`. Every other command carrying it is refused
//     (TERMINAL_READ_ONLY_UNSUPPORTED) before it can run, so a read-only
//     request never runs writable. Read-only also confines READS to the
//     working directory (SECURITY_REVIEW_PLAN.md B13): Claude Code's
//     `--restricted` does; Codex has no way to, so a read-only `codex` is
//     refused on every platform until an OS read jail exists for it, and a
//     read-only `claude` on Windows until the jail is verified there.
//   - The CLI's argv is built HERE from a fixed shape. The caller may choose a
//     model (and Claude's effort level) and nothing else; every other caller
//     flag is dropped.
//   - The executable is resolved through this agent's own PATH lookup and must
//     be an absolute path outside the session's working directory, so a binary
//     shipped inside the repository can never stand in for the CLI.
//
// terminal-service sends `readOnly` only to a device that reports
// capabilities.readOnlySessions (systemInfo.go); an older agent would compute
// a canonical without the field and reject the command as a bad signature
// rather than run it writable.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"cloud.google.com/go/pubsub/v2"
)

const (
	// readOnlyUnsupportedErrorCode is the errorCode of every read-only refusal
	// (shared-constants TERMINAL_ERROR_CODES.TERMINAL_READ_ONLY_UNSUPPORTED).
	readOnlyUnsupportedErrorCode = "TERMINAL_READ_ONLY_UNSUPPORTED"

	// The only commands with an enforced read-only mode (shared-constants
	// READ_ONLY_CLI_COMMANDS). Compared EXACTLY: no path, no launcher suffix,
	// no other casing, no whitespace, no inline arguments.
	readOnlyClaudeCommand = "claude"
	readOnlyCodexCommand  = "codex"

	// readOnlyStartType is the one command type that may carry readOnly.
	readOnlyStartType = "session_start"
)

// readOnlyHostOS is the platform the read-only policy is evaluated for. A
// variable only so tests can exercise the Windows refusal on any host.
var readOnlyHostOS = runtime.GOOS

// readOnlySessionError is a read-only start the device refused. Typed so the
// session_start dispatcher can publish it as TERMINAL_READ_ONLY_UNSUPPORTED
// rather than as an ordinary start failure.
type readOnlySessionError struct {
	reason string
}

func (e *readOnlySessionError) Error() string {
	return "read-only session refused: " + e.reason
}

// readOnlySessionErrorFrom returns the read-only refusal wrapped in err, or nil.
func readOnlySessionErrorFrom(err error) *readOnlySessionError {
	var roErr *readOnlySessionError
	if errors.As(err, &roErr) {
		return roErr
	}
	return nil
}

/* --------------------------------------------------------------------------
   Policy: which commands may run read-only
   -------------------------------------------------------------------------- */

// readOnlyCommandRefusal returns why cmd, which asks to run read-only, cannot,
// or "" when it may (or does not ask). The receive loop applies it to EVERY
// command before dispatch (refuseReadOnlyCommand) and the session dispatcher
// applies it again, so no route can run a read-only request writable.
func readOnlyCommandRefusal(cmd commandMsg) string {
	if !cmd.ReadOnly {
		return ""
	}
	if cmd.Type != readOnlyStartType {
		typ := cmd.Type
		if typ == "" {
			typ = "execute"
		}
		return fmt.Sprintf("only a session_start can run read-only; a %s command has no enforced read-only mode", typ)
	}
	return readOnlySessionStartRefusal(cmd.Command, cmd.Tty, cmd.ConversationID)
}

// readOnlySessionStartRefusal is the session_start half of the policy, shared
// by the wire gate and SessionManager.startSession (the last check before the
// spawn).
func readOnlySessionStartRefusal(command string, tty bool, conversationID string) string {
	switch {
	case tty:
		return "a read-only session cannot run under a TTY/PTY"
	case conversationID != "":
		return "a read-only session cannot resume a conversation; it must start fresh"
	case command == readOnlyCodexCommand:
		// Its read-only sandbox stops writes and the network, not reads: a
		// prompt-injected repository could pull any file the computer's user
		// can read (~/.ssh, a cloud credential) into the transcript.
		return "read-only Codex sessions are refused: Codex cannot confine its reads to the " +
			"working directory (no read jail for it yet)"
	case command != readOnlyClaudeCommand:
		return "only the bare command `claude` has an enforced read-only mode " +
			"(no path, launcher suffix, shell wrapper or inline arguments)"
	case readOnlyHostOS == "windows":
		// --restricted's confinement is verified on macOS; Windows paths
		// (case folding, 8.3 short names, junctions) are not verified yet.
		return "read-only Claude Code sessions are not supported on Windows yet " +
			"(its read jail is not verified there)"
	}
	return ""
}

// readOnlyRefusalResult is the frame published for a refused read-only
// command: the ordinary rejection shape (redacted command/args, requested
// cwd, boot id; for a session-routed command its family's error type and the
// session id — `session_error` for a session_start) with status "error" and
// errorCode TERMINAL_READ_ONLY_UNSUPPORTED, so consumers never parse Output.
func readOnlyRefusalResult(cmd commandMsg, agentID, reason string) resultMsg {
	res := makeRejectionResult(cmd, agentID, "error", readOnlyUnsupportedErrorCode,
		"Read-only session refused: "+reason)
	res.ErrorCode = readOnlyUnsupportedErrorCode
	return res
}

// refuseReadOnlyCommand refuses a command that asks to run read-only and
// cannot (readOnlyCommandRefusal). Returns true when it handled (refused and
// settled) the message. Bracketed like the other pre-admission rejections:
// once a replacement process has sealed the drain the message is Nacked so
// the replacement answers it.
func refuseReadOnlyCommand(ctx context.Context, topic *pubsub.Publisher, m *pubsub.Message, cmd commandMsg, cfg *Config) bool {
	reason := readOnlyCommandRefusal(cmd)
	if reason == "" {
		return false
	}
	fmt.Printf("%s[read-only] Refused type=%q id=%s: %s%s\n", colorYellow, cmd.Type, cmd.ID, reason, colorReset)
	release, allowed := beginTrackedRejectionPublish()
	if !allowed {
		if m != nil {
			m.Nack()
		}
		return true
	}
	defer release()
	agentID := ""
	if cfg != nil {
		agentID = cfg.AgentID
	}
	err := publishMsg(ctx, topic, readOnlyRefusalResult(cmd, agentID, reason))
	if m != nil {
		if err != nil {
			m.Nack()
		} else {
			m.Ack()
		}
	}
	return true
}

/* --------------------------------------------------------------------------
   Argv: the fixed read-only shapes
   -------------------------------------------------------------------------- */

// readOnlyClaudeBaseArgs is Claude Code's read-only launch shape, ahead of the
// caller's model / effort. The same bidirectional stream-json framing as the
// writable session (buildClaudeInteractiveArgs), so the prompt still travels
// on stdin as NDJSON, and:
//
//   - --restricted (the READ jail, SECURITY_REVIEW_PLAN.md B13): confines the
//     file tools to the working directory — the snapshot under review — with
//     symlinks resolved, so a prompt-injected repository cannot have Read /
//     Grep / Glob pull ~/.ssh or a cloud credential into the transcript. It
//     also refuses bypassPermissions, so --dangerously-skip-permissions is
//     GONE: the default permission mode reads inside the working directory
//     without asking.
//   - --permission-prompts none: anything that would still ask (a read
//     outside the jail) is DENIED at once — the session never blocks on a
//     prompt nobody will answer. Requires a Claude Code that knows both flags;
//     checkReadOnlyClaudeJail refuses the start on one that does not.
//   - --safe-mode: no hooks, plugins, skills, custom agents/commands or
//     CLAUDE.md — none of the repository's extensions load.
//   - --strict-mcp-config with no --mcp-config: no MCP server at all.
//   - --setting-sources "": no settings FILE at all (managed policy still
//     applies). In a non-interactive (piped) session Claude Code skips the
//     workspace-trust dialog, so the repository's own .claude/settings.json /
//     settings.local.json would load — and an `apiKeyHelper` there is a shell
//     command Claude runs at startup, which --safe-mode does not cover. The
//     device owner's user settings are dropped too: a helper (or any command
//     setting) there that names a relative path, `node ./get-key.js`, resolves
//     in the repository. Verified on Claude Code 2.1.260: a user apiKeyHelper
//     runs with `--setting-sources user` and not with "", and the stored
//     sign-in (OAuth / keychain) still works with "". An owner who signs in
//     ONLY through an apiKeyHelper cannot run a read-only review (the pass
//     fails "Not logged in"); ANTHROPIC_API_KEY in the environment still works.
//   - --tools Read,Grep,Glob: the only built-in tools.
//   - --disallowedTools: the write, shell, web and sub-agent tools, denied
//     even if a later --tools named them (the deny list wins).
//
// Verified live on Claude Code 2.1.260 with exactly this argv: the init event
// lists only Glob, Grep and Read, no MCP server, permission mode "default",
// and a turn reading a file in the working directory completes.
var readOnlyClaudeBaseArgs = []string{
	"--output-format", "stream-json",
	"--input-format", "stream-json",
	"--verbose",
	"--include-partial-messages",
	"--restricted",
	"--permission-prompts", "none",
	"--safe-mode",
	"--strict-mcp-config",
	"--setting-sources", "",
	"--tools", "Read,Grep,Glob",
	"--disallowedTools", "Bash,Edit,Write,MultiEdit,NotebookEdit,WebFetch,WebSearch,Task,Agent",
}

// readOnlyCodexBaseArgs is Codex's read-only launch shape, ahead of the
// caller's model. UNUSED while the policy refuses read-only Codex (it cannot
// confine reads); kept, with its tests, for when an OS read jail exists.
//
// The shape, ahead of the caller's model and the `-` stdin placeholder (the
// prompt still travels on stdin, exactly as buildCodexInteractiveArgs sends
// it):
//
//   - --sandbox read-only: the OS sandbox blocks every write and the network;
//     reads work. Verified live on codex-cli 0.154.0 (macOS).
//   - --ignore-user-config: no $CODEX_HOME/config.toml — no MCP server,
//     profile, hook or provider override from it, and no project trust, so the
//     repository's own .codex/ config, hooks and rules stay disabled.
//   - --ignore-rules: no user or project execpolicy `.rules`. Codex runs a
//     command an allow rule matches OUTSIDE the sandbox ("Commands are run
//     outside the sandbox if they are approved by the user, or match an
//     existing rule that allows it to run unrestricted" — Codex's own
//     instructions), so an "always allow" the owner once gave a command would
//     otherwise escape read-only.
//   - --skip-git-repo-check: the review runs on a snapshot, not a checkout.
//
// This exact argv was verified live on codex-cli 0.154.0 to complete a turn.
// NEVER --dangerously-bypass-approvals-and-sandbox, --full-auto, -c/--config,
// -p/--profile or --add-dir: every one of them can widen the sandbox.
var readOnlyCodexBaseArgs = []string{
	"exec",
	"--json",
	"--sandbox", "read-only",
	"--ignore-user-config",
	"--ignore-rules",
	"--skip-git-repo-check",
}

// The ONLY caller flags a read-only start keeps, each with exactly one value
// in the `--flag value` form or, for a long flag, the `--flag=value` form.
var (
	readOnlyClaudeCallerFlags = map[string]bool{"--model": true, "--effort": true}
	readOnlyCodexCallerFlags  = map[string]bool{"-m": true, "--model": true}
)

// claudeReadOnlyExtraValuedFlags are Claude Code options that take a value
// but are not in claudeInteractiveValuedFlags (the writable builder forwards
// them, value and all, without needing to know). A read-only start DROPS
// them, so it must know which token is their value, or that value would be
// read as a prompt word. A list-valued flag (`--allowedTools a b`) consumes
// one token here, exactly as the writable classifier does for its own list.
var claudeReadOnlyExtraValuedFlags = map[string]bool{
	"--add-dir": true, "--allowedTools": true, "--allowed-tools": true,
	"--disallowedTools": true, "--disallowed-tools": true, "--tools": true,
	"--betas": true, "--file": true, "--input-format": true, "--output-format": true,
	"--plugin-dir": true, "--plugin-url": true, "-n": true, "--name": true,
	"--autocompact": true, "--environment": true, "--permission-prompts": true,
	"--system-prompt-file": true, "--append-system-prompt-file": true,
	"--system-prompt-snapshot": true, "--remote-control-session-name-prefix": true,
	"--max-turns": true,
}

// codexReadOnlyExtraValuedFlags are the codex options that take a value but
// are not in codexExecValuedFlags: the sandbox / approval overrides that
// sanitizeCodexExecArgs strips (with their value) on the writable path, and
// --thread-source.
var codexReadOnlyExtraValuedFlags = map[string]bool{
	"--sandbox": true, "-s": true,
	"--approval-policy": true, "--ask-for-approval": true, "-a": true,
	"--thread-source": true,
}

// readOnlyFlagValueMaxLen bounds a kept model / effort value.
const readOnlyFlagValueMaxLen = 128

// readOnlyFlagValueOK accepts a model or effort name: 1–128 characters of
// letters, digits and . _ - : / @ [ ], starting with a letter or digit. A
// model alias (`opus`, `sonnet[1m]`), a full id (`claude-opus-4-5-20251101`,
// `gpt-5.1-codex`) or a provider-qualified id (`us.anthropic.claude-…-v1:0`)
// fits. A value that could be read as another flag, or that a Windows `.cmd`
// shim's cmd.exe pass could re-split or expand (space, quote, & | < > ^ %), is
// refused, and the flag is dropped with it.
func readOnlyFlagValueOK(value string) bool {
	if value == "" || len(value) > readOnlyFlagValueMaxLen {
		return false
	}
	for i, r := range value {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && !strings.ContainsRune("._-:/@[]", r) {
			return false
		}
	}
	return true
}

// readOnlyOptionNamePattern is what a caller token's name (the part before
// any `=`) must look like to be treated as an option at all: one or two
// dashes, a letter, then letters, digits, `_` or `-`. Anything else that
// happens to start with "-" — a brief opening with `---` front matter, `-1`,
// a sentence — is prompt text, so it is delivered rather than dropped, and the
// dropped-flag log below can only ever contain option NAMES.
var readOnlyOptionNamePattern = regexp.MustCompile(`^--?[A-Za-z][A-Za-z0-9_-]*$`)

// readOnlyDroppedFlagName is how a dropped caller flag is named in the log:
// its name only, never its value. name has already matched
// readOnlyOptionNamePattern (so it carries no `=value`); a single-dash token
// is cut to its option letter (`-pSECRET` → `-p`), the result is bounded and
// passed through the secret redactor.
func readOnlyDroppedFlagName(name string) string {
	if !strings.HasPrefix(name, "--") && len(name) > 2 {
		name = name[:2]
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return redactSensitiveData(name)
}

// readOnlyArgvPartition is one caller argv, classified for a read-only start.
type readOnlyArgvPartition struct {
	kept    []string // allowlisted flags (with their values), in caller order
	prompt  []string // positional words: the prompt, delivered on stdin
	dropped []string // names of every caller flag that was dropped
}

// partitionReadOnlyCallerArgs splits a caller's argv into the allowlisted
// flags it may keep, the prompt words, and the dropped flags.
//
//   - allowed: the allowlisted flag names, each consuming exactly one value.
//   - valued: every OTHER flag the CLI knows to take a value; the token after
//     it is that value and is dropped with it, never read as a prompt word.
//   - printFlag: optional classifier for Claude's -p/--print (stripped, an
//     inline `--print=text` prompt kept) so the prompt matches the writable
//     builder's.
//
// A token whose name matches readOnlyOptionNamePattern is an option: kept if
// allowlisted, dropped otherwise. `-` (the stdin placeholder; the builder adds
// its own) is dropped, and `--` ends the options: every token after it is a
// prompt word. Every other token is a prompt word.
func partitionReadOnlyCallerArgs(args []string, allowed, valued func(string) bool, printFlag func(string) (bool, string)) readOnlyArgvPartition {
	var p readOnlyArgvPartition
	for i := 0; i < len(args); i++ {
		a := args[i]
		if printFlag != nil {
			if isPrint, inline := printFlag(a); isPrint {
				if inline != "" {
					p.prompt = append(p.prompt, inline)
				}
				name, _, _ := strings.Cut(a, "=")
				p.dropped = append(p.dropped, readOnlyDroppedFlagName(name))
				continue
			}
		}
		if a == "-" {
			p.dropped = append(p.dropped, "-")
			continue
		}
		if a == "--" {
			p.dropped = append(p.dropped, "--")
			p.prompt = append(p.prompt, args[i+1:]...)
			break
		}
		name, value, inline := strings.Cut(a, "=")
		if !readOnlyOptionNamePattern.MatchString(name) {
			p.prompt = append(p.prompt, a)
			continue
		}
		switch {
		case inline && strings.HasPrefix(name, "--") && allowed(name):
			if readOnlyFlagValueOK(value) {
				p.kept = append(p.kept, a)
			} else {
				p.dropped = append(p.dropped, readOnlyDroppedFlagName(name)+" (invalid value)")
			}
		case !inline && allowed(name):
			// The next token is the value, taken unconditionally — as the CLIs'
			// own parsers take a required option-argument even when it starts
			// with "-" — so a value is never re-read as a flag (and never
			// logged as one).
			if i+1 >= len(args) {
				p.dropped = append(p.dropped, readOnlyDroppedFlagName(name)+" (missing value)")
				continue
			}
			i++
			if readOnlyFlagValueOK(args[i]) {
				p.kept = append(p.kept, name, args[i])
			} else {
				p.dropped = append(p.dropped, readOnlyDroppedFlagName(name)+" (invalid value)")
			}
		default:
			p.dropped = append(p.dropped, readOnlyDroppedFlagName(name))
			if !inline && valued(name) && i+1 < len(args) {
				i++ // its value goes with it, unlogged
			}
		}
	}
	return p
}

// buildReadOnlyClaudeArgs builds Claude Code's read-only argv: the fixed shape
// plus the caller's --model / --effort, and nothing else. Returns the argv,
// the prompt (positional words, delivered on stdin as NDJSON exactly as the
// writable session does) and the names of the caller flags it dropped.
func buildReadOnlyClaudeArgs(args []string) ([]string, string, []string) {
	p := partitionReadOnlyCallerArgs(args,
		func(name string) bool { return readOnlyClaudeCallerFlags[name] },
		func(name string) bool {
			return claudeInteractiveValuedFlags[name] || claudeReadOnlyExtraValuedFlags[name]
		},
		claudePrintFlagPrompt)
	out := make([]string, 0, len(readOnlyClaudeBaseArgs)+len(p.kept))
	out = append(out, readOnlyClaudeBaseArgs...)
	out = append(out, p.kept...)
	return out, strings.Join(p.prompt, " "), p.dropped
}

// buildReadOnlyCodexArgs builds Codex's read-only argv: the fixed shape plus
// the caller's -m / --model, then `-` so the prompt is read from stdin. A
// leading `exec` (the writable path accepts one) is skipped; a subcommand word
// (`resume`, `review`, …) is NOT honoured — it is a prompt word, so a
// read-only Codex run always starts a fresh top-level turn.
func buildReadOnlyCodexArgs(args []string) ([]string, string, []string) {
	if len(args) > 0 && strings.EqualFold(args[0], "exec") {
		args = args[1:]
	}
	p := partitionReadOnlyCallerArgs(args,
		func(name string) bool { return readOnlyCodexCallerFlags[name] },
		func(name string) bool {
			return codexExecValuedFlags[name] || codexReadOnlyExtraValuedFlags[name]
		},
		nil)
	out := make([]string, 0, len(readOnlyCodexBaseArgs)+len(p.kept)+1)
	out = append(out, readOnlyCodexBaseArgs...)
	out = append(out, p.kept...)
	out = append(out, "-")
	return out, strings.Join(p.prompt, " "), p.dropped
}

// buildReadOnlySessionArgs routes a read-only start to its CLI's builder.
// Returns the argv, the stdin prompt (nil when there is none, matching
// buildInteractiveCLIArgs) and the dropped caller flag names.
func buildReadOnlySessionArgs(command string, args []string) ([]string, *string, []string, error) {
	var cliArgs, dropped []string
	var prompt string
	switch command {
	case readOnlyClaudeCommand:
		cliArgs, prompt, dropped = buildReadOnlyClaudeArgs(args)
	case readOnlyCodexCommand:
		cliArgs, prompt, dropped = buildReadOnlyCodexArgs(args)
	default:
		return nil, nil, nil, &readOnlySessionError{reason: fmt.Sprintf("%q has no read-only argv", command)}
	}
	if prompt == "" {
		return cliArgs, nil, dropped, nil
	}
	return cliArgs, &prompt, dropped, nil
}

/* --------------------------------------------------------------------------
   Executable: never one from the session's working directory
   -------------------------------------------------------------------------- */

// checkReadOnlyExecutable refuses an executable a read-only session must not
// run. executable is what this agent's own PATH lookup resolved for command
// (resolveExecutable, the same resolution a writable start uses). It must be:
//
//   - ABSOLUTE. A bare name means the lookup found no installation (exec
//     would then search again at spawn); a relative one is what Go reports as
//     exec.ErrDot — a match in "." or another relative PATH entry, or the
//     current directory Windows searches implicitly.
//   - OUTSIDE the session's working directory — the untrusted repository —
//     both as the path names it and as the file its symlinks lead to, so an
//     absolute PATH entry that points into the repository cannot supply it.
func checkReadOnlyExecutable(command, executable, cwd string) error {
	if !filepath.IsAbs(executable) {
		return &readOnlySessionError{reason: fmt.Sprintf(
			"`%s` did not resolve to an absolute path through this computer's PATH", command)}
	}
	// An npm `.cmd` / `.bat` launcher cannot be started by CreateProcess
	// directly, and routing a read-only argv through cmd.exe would add a
	// second, differently-quoted parser in front of the CLI. A read-only
	// session needs the native executable (Claude Code's own installer).
	if isWindowsShimPath(executable) {
		return &readOnlySessionError{reason: fmt.Sprintf(
			"`%s` is installed as a Windows batch launcher (%s); a read-only session needs the native executable", command, filepath.Base(executable))}
	}
	workDir, err := readOnlySessionWorkDir(cwd)
	if err != nil {
		return &readOnlySessionError{reason: "the session's working directory could not be resolved"}
	}
	real, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return &readOnlySessionError{reason: fmt.Sprintf("the resolved `%s` executable could not be inspected", command)}
	}
	for _, candidate := range []string{executable, real} {
		if pathWithinDir(candidate, workDir) {
			return &readOnlySessionError{reason: fmt.Sprintf(
				"`%s` resolved to a file inside the session's working directory; a read-only session never runs a binary from there", command)}
		}
	}
	return nil
}

/* --------------------------------------------------------------------------
   The read jail: only a Claude Code that has one
   -------------------------------------------------------------------------- */

// readOnlyClaudeJailFlags are the flags the read jail rests on
// (readOnlyClaudeBaseArgs). A Claude Code that does not list both in its
// --help predates the jail.
var readOnlyClaudeJailFlags = []string{"--restricted", "--permission-prompts"}

// readOnlyClaudeHelpProbe runs `<executable> --help` and returns its output;
// ok=false is inconclusive (spawn failure, timeout, non-zero exit). A variable
// so tests can answer for a CLI without spawning one.
var readOnlyClaudeHelpProbe = func(executable string, env []string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), cliAgentModelProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--help")
	hideWindow(cmd)
	cmd.Env = env
	// Never the repository: the executable's own directory is outside it
	// (checkReadOnlyExecutable).
	cmd.Dir = filepath.Dir(executable)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(out), true
}

// readOnlyClaudeJailCache remembers a verdict per installed binary (path, size
// and modification time), so the probe runs once per Claude Code install, not
// once per review — and again after an upgrade replaces the file.
var readOnlyClaudeJailCache sync.Map // string → bool

// checkReadOnlyClaudeJail refuses a read-only Claude start unless the
// installed Claude Code has the read jail. Without the check, a Claude Code
// that predates `--restricted` would exit on an unknown option — still never
// unjailed, but as an opaque start failure instead of the refusal the job
// reads (TERMINAL_READ_ONLY_UNSUPPORTED). An inconclusive probe refuses too.
func checkReadOnlyClaudeJail(executable string, env []string) error {
	key := ""
	if info, err := os.Stat(executable); err == nil {
		key = fmt.Sprintf("%s\x00%d\x00%d", executable, info.Size(), info.ModTime().UnixNano())
		if ok, cached := readOnlyClaudeJailCache.Load(key); cached && ok.(bool) {
			return nil
		}
	}
	help, ok := readOnlyClaudeHelpProbe(executable, env)
	if !ok {
		return &readOnlySessionError{reason: "could not confirm that this computer's Claude Code confines reads " +
			"(`claude --help` did not answer)"}
	}
	for _, flag := range readOnlyClaudeJailFlags {
		if !strings.Contains(help, flag) {
			return &readOnlySessionError{reason: fmt.Sprintf(
				"this computer's Claude Code has no %s option, so it cannot confine reads to the "+
					"working directory; update Claude Code", flag)}
		}
	}
	if key != "" {
		readOnlyClaudeJailCache.Store(key, true)
	}
	return nil
}

// readOnlySessionWorkDir is the directory the child will run in: cwd made
// absolute the way exec resolves a relative proc.Dir (against this process's
// own working directory), or this process's working directory when cwd is
// empty (proc.Dir is then left unset and the child inherits it).
func readOnlySessionWorkDir(cwd string) (string, error) {
	if cwd == "" {
		return os.Getwd()
	}
	return filepath.Abs(cwd)
}

// pathWithinDir reports whether target lies inside dir (at any depth). It
// answers true if EITHER test does — a false "inside" only refuses a start,
// a false "outside" would run a repository binary:
//
//   - lexically, against dir and dir's symlink-resolved form (case-folded on
//     Windows by isPathSafeUnder);
//   - by identity: every ancestor directory of target is compared to dir with
//     os.SameFile, which sees through case differences, 8.3 short names and
//     symlinked spellings of the same directory.
func pathWithinDir(target, dir string) bool {
	dirs := []string{filepath.Clean(dir)}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dirs = append(dirs, real)
	}
	for _, d := range dirs {
		if isPathSafeUnder(target, d) {
			return true
		}
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return false
	}
	for p := filepath.Dir(target); ; {
		if info, err := os.Stat(p); err == nil && os.SameFile(info, dirInfo) {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// readOnlyChildEnv keeps a read-only CLI, and everything it starts, from
// resolving a program out of the untrusted repository it runs in:
//
//   - NoDefaultCurrentDirectoryInExePath: on Windows, cmd.exe — which runs an
//     npm-installed `claude.cmd` shim, whose bare `node` it must then find —
//     and the process search behind it otherwise try the CURRENT directory
//     first. Other platforms ignore the variable.
//   - the runtime loader and preload variables are dropped
//     (withoutRuntimeLoaderVars): `NODE_OPTIONS=--require=./x.js` makes an
//     npm launcher run a repository module before the CLI reads a single
//     read-only flag, and LD_PRELOAD / DYLD_INSERT_LIBRARIES, a shell's
//     BASH_ENV, PYTHONPATH and their kin do the same for everything the CLI
//     starts. A review needs none of them.
//   - PATH keeps only absolute entries outside workDir (readOnlySessionPath):
//     an empty or relative entry ("", ".", "bin") resolves against the
//     repository, and so does any entry inside it, so a launcher's
//     `#!/usr/bin/env node` could otherwise run a repository-supplied
//     interpreter before the CLI's tool restrictions apply.
func readOnlyChildEnv(env []string, workDir string) []string {
	env = withoutRuntimeLoaderVars(env)
	env = setEnvVar(env, "NoDefaultCurrentDirectoryInExePath", "1")
	if path, ok := envVarValue(env, "PATH"); ok {
		env = setEnvVar(env, "PATH", readOnlySessionPath(path, workDir))
	}
	return env
}

// checkReadOnlyStateDir refuses a read-only start whose CLI would write its
// own state — Codex's session rollouts under CODEX_HOME, Claude Code's
// transcripts under CLAUDE_CONFIG_DIR and ~/.claude.json — inside the
// directory under review: the sandbox and the tool limits govern what the
// MODEL may do, not what the CLI process persists. The state directory is
// the CLI's variable when set, else its default under the home directory;
// it, and the home directory, must be absolute and outside workDir. A state
// directory that cannot be worked out is refused too.
func checkReadOnlyStateDir(command string, env []string, workDir string) error {
	homeVar := "HOME"
	if runtime.GOOS == "windows" {
		homeVar = "USERPROFILE"
	}
	home, _ := envVarValue(env, homeVar)
	stateVar, stateDefault := "", ""
	switch {
	case isCodexCommand(command):
		stateVar, stateDefault = "CODEX_HOME", ".codex"
	case isClaudeCommand(command):
		stateVar, stateDefault = "CLAUDE_CONFIG_DIR", ".claude"
	default:
		return nil
	}
	refuse := func(what, dir string) error {
		return &readOnlySessionError{reason: fmt.Sprintf(
			"`%s` would write its own state into %s (%q), which must be an absolute directory outside the reviewed checkout", command, what, dir)}
	}
	unsafe := func(dir string) bool {
		return dir == "" || !filepath.IsAbs(dir) || (workDir != "" && readOnlyPathEntryInside(dir, workDir))
	}
	if unsafe(home) {
		return refuse(homeVar, home)
	}
	if dir, ok := envVarValue(env, stateVar); ok && dir != "" {
		if unsafe(dir) {
			return refuse(stateVar, dir)
		}
		return nil
	}
	if dir := filepath.Join(home, stateDefault); unsafe(dir) {
		return refuse("the default "+stateDefault, dir)
	}
	return nil
}

// runtimeLoaderVars are dropped from a read-only child's environment: each
// makes a runtime (or the dynamic linker, or a shell) load code it names
// before or around the program it starts. Every LD_* and DYLD_* name goes too.
var runtimeLoaderVars = []string{
	"NODE_OPTIONS", "NODE_PATH",
	"BASH_ENV", "ENV", "ZDOTDIR",
	"PYTHONPATH", "PYTHONSTARTUP", "PYTHONHOME",
	"RUBYOPT", "RUBYLIB",
	"PERL5OPT", "PERL5LIB", "PERLLIB",
	"JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "JDK_JAVA_OPTIONS",
	"GIT_EXEC_PATH",
}

func withoutRuntimeLoaderVars(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		separator := strings.IndexByte(entry, '=')
		if separator <= 0 {
			out = append(out, entry)
			continue
		}
		if isRuntimeLoaderVar(entry[:separator]) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isRuntimeLoaderVar(name string) bool {
	upper := name
	if runtime.GOOS == "windows" {
		upper = strings.ToUpper(name)
	}
	if strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "DYLD_") {
		return true
	}
	for _, v := range runtimeLoaderVars {
		if upper == v {
			return true
		}
	}
	return false
}

// readOnlySessionPath is PATH without the entries a read-only session must
// never search: empty and relative ones, and any that is — or, through a
// symlink, resolves to — workDir or a directory inside it. Order is kept.
func readOnlySessionPath(path, workDir string) string {
	kept := make([]string, 0, 16)
	for _, entry := range filepath.SplitList(path) {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		if workDir != "" && readOnlyPathEntryInside(entry, workDir) {
			continue
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		// An empty PATH is no safer: execvp reads it as one empty entry, the
		// current directory. Fall back to the system's own directories.
		return readOnlyFallbackPath()
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

func readOnlyFallbackPath() string {
	if runtime.GOOS == "windows" {
		root := os.Getenv("SystemRoot")
		if root == "" || !filepath.IsAbs(root) {
			root = `C:\Windows`
		}
		return strings.Join([]string{filepath.Join(root, "System32"), root}, ";")
	}
	return "/usr/bin:/bin"
}

func readOnlyPathEntryInside(entry, workDir string) bool {
	if pathWithinDir(entry, workDir) {
		return true
	}
	if real, err := filepath.EvalSymlinks(entry); err == nil && pathWithinDir(real, workDir) {
		return true
	}
	entryInfo, err1 := os.Stat(entry)
	dirInfo, err2 := os.Stat(workDir)
	return err1 == nil && err2 == nil && os.SameFile(entryInfo, dirInfo)
}

// envVarValue is key's value in env (case-insensitively on Windows, where
// the environment is), and whether it is set.
func envVarValue(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		separator := strings.IndexByte(env[i], '=')
		if separator < 0 {
			continue
		}
		name := env[i][:separator]
		if name == key || (runtime.GOOS == "windows" && strings.EqualFold(name, key)) {
			return env[i][separator+1:], true
		}
	}
	return "", false
}
