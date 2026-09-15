// cliagent_usage_antigravity_command.go — "will spawning this start `agy`?"
//
// Two questions about a command, two predicates, neither derived from the other:
//
//   - isAntigravityCommand(command) — is this PROGRAM the Antigravity CLI? It
//     drives argv shaping, resident-agent classification and the direct-exec
//     route on Windows, and lives with its siblings in session.go.
//   - commandRunsAntigravity(command, args) — will SPAWNING this start an `agy`
//     process, and therefore a readable language server? It drives quota-capture
//     arming only (cliagent_usage_antigravity_capture.go), and must therefore see
//     through every wrapper transport terminal-service puts on the wire.
//
// The second is why this file exists. The classifier used to unwrap POSIX
// `bash -c` only, so a Windows device — where terminal-service ships everything
// as `powershell -EncodedCommand <base64>` — never armed capture at all, and a
// green CLI-maintenance smoke was still followed by a day-old observedAt.
//
// REDACTION: the decoded script is a classification input and nothing else. It
// is a user's prompt, and may carry credentials, tokens and file contents. It is
// never logged, never persisted, never attached to a result, and never returned
// to a caller — the only thing that leaves this file is a bool.
package main

import (
	"encoding/base64"
	"regexp"
	"strings"
)

// antigravityClassifyMaxPayloadBytes caps the script this file will read. A
// payload larger than this classifies as "not antigravity" rather than growing
// the decode budget: the cost of being wrong is one unarmed capture (stale
// freshness), while the cost of an unbounded decode on a device the user is
// working on is real. terminal-service's own -EncodedCommand argument path caps
// out at encodedCommandFallbackThreshold (30000 chars), so a real agy
// invocation is orders of magnitude below this.
const antigravityClassifyMaxPayloadBytes = 256 * 1024

// antigravityFileModeBase64 matches the ONE nested base64 literal
// terminal-service's `scriptMode: "file"` launcher carries
// (commandNormalize.util.js → buildFileModeInvocation): the outer
// -EncodedCommand decodes to a launcher that writes the REAL script to a temp
// file from this literal and runs it with `-File`. Without peeling it, every
// file-mode run — which is what a long prompt becomes — classifies as the
// launcher rather than as agy.
var antigravityFileModeBase64 = regexp.MustCompile(`FromBase64String\('([A-Za-z0-9+/=]*)'\)`)

// antigravityPosixFileModeBase64 is the same launcher's POSIX half: the script
// is base64 in a `printf '%s' '<b64>' | base64 -d` pipeline feeding a temp file.
var antigravityPosixFileModeBase64 = regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]*)'`)

// commandRunsAntigravity reports whether SPAWNING command+args starts the
// Antigravity CLI — i.e. whether an `agy` language server will exist for the
// life of that child, and therefore whether the run-scoped quota capture should
// be armed.
//
// Distinct from isAntigravityCommand, which asks whether the CLI ROUTER should
// treat the command as Antigravity and shape its argv. terminal-service ships
// operator-joined commands as `bash -c "agy …"` on Unix and
// `powershell -EncodedCommand <base64>` on Windows, where the base command is
// the shell: the router deliberately leaves those to shapeShellWrappedPTYArgs,
// but the process they spawn is still agy and still holds the only readable copy
// of the quota.
//
// Being wrong in the positive direction costs a poller that finds no server and
// exits on its own bounds (200 attempts / 15 min); being wrong in the negative
// direction costs the whole point of the capture. The scan is therefore
// deliberately generous about WHERE in a script the program may appear and
// strict about it being a program rather than a mention — `git log --grep agy`
// must not match.
func commandRunsAntigravity(command string, args []string) bool {
	if script, ok := wrapperScriptPayload(command, args); ok {
		return scriptSpawnsAntigravity(script, true)
	}
	return isAntigravityCommand(command)
}

// scriptSpawnsAntigravity reports whether any statement in an interpreter script
// launches `agy`. allowNested permits exactly ONE descent into the file-mode
// launcher's inner base64 literal — never a general recursive unwrap, which an
// adversarial payload could use to make classification unbounded.
func scriptSpawnsAntigravity(script string, allowNested bool) bool {
	if script == "" || len(script) > antigravityClassifyMaxPayloadBytes {
		return false
	}
	if allowNested {
		if inner, ok := fileModeLauncherScript(script); ok {
			return scriptSpawnsAntigravity(inner, false)
		}
	}
	for _, statement := range splitScriptStatements(script) {
		if isAntigravityCommand(leadingProgram(statement)) {
			return true
		}
	}
	return false
}

// fileModeLauncherScript decodes the inner script of terminal-service's
// file-mode launcher, for both halves of buildFileModeInvocation. Returns false
// for anything that is not that exact launcher shape: the `-File` / temp-file
// execution is required, so a script that merely happens to call
// FromBase64String is left to the normal statement scan.
func fileModeLauncherScript(script string) (string, bool) {
	if strings.Contains(script, "-File") {
		if m := antigravityFileModeBase64.FindStringSubmatch(script); len(m) == 2 {
			// PowerShell writes its launcher literal as UTF-16LE, matching
			// encodeForPowerShell.
			decoded, err := decodeBase64PowerShellStrict(m[1])
			if err != nil {
				return "", false
			}
			return decoded, true
		}
	}
	if strings.Contains(script, "base64 -d") && strings.Contains(script, "mktemp") {
		if m := antigravityPosixFileModeBase64.FindStringSubmatch(script); len(m) == 2 {
			decoded, err := base64.StdEncoding.DecodeString(m[1])
			if err != nil {
				return "", false
			}
			return string(decoded), true
		}
	}
	return "", false
}

// splitScriptStatements cuts a script at the separators that START a new
// program: `;`, `&&`, `||`, `|`, a newline, and PowerShell's `&` call operator
// (which on POSIX is the background separator — either way a program token
// follows it). Separators inside quotes are literal text and do not split, so
// `git commit -m 'a && b'` stays one statement.
//
// Cheap on purpose: one pass, no allocation per character, and the payload is
// already capped by the caller.
func splitScriptStatements(script string) []string {
	statements := make([]string, 0, 4)
	start := 0
	var quote byte
	for i := 0; i < len(script); i++ {
		c := script[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '&', '|':
			statements = append(statements, script[start:i])
			// `&&` and `||` are one separator, not two empty statements.
			if i+1 < len(script) && script[i+1] == c {
				i++
			}
			start = i + 1
		case ';', '\n', '\r':
			statements = append(statements, script[start:i])
			start = i + 1
		}
	}
	return append(statements, script[start:])
}

// leadingProgram returns the program a statement runs, or "" when it runs none.
// Env-var prefixes (`FOO=bar agy …`) and PowerShell's Start-Process / its
// -FilePath flag are stepped over, and one pair of outer quotes is stripped so
// `& "C:\Program Files\agy.cmd"` names agy rather than a quoted path.
func leadingProgram(statement string) string {
	rest := statement
	for range 4 { // bounded: at most an env prefix, Start-Process and -FilePath
		token, tail := nextScriptToken(rest)
		if token == "" {
			return ""
		}
		switch {
		case strings.EqualFold(token, "start-process"), strings.EqualFold(token, "start"):
			rest = tail
		case strings.EqualFold(token, "-filepath"):
			rest = tail
		// A POSIX env assignment prefix is not the program. `$env:X=1` is a
		// whole PowerShell statement of its own, so it needs no case here.
		case isEnvAssignmentToken(token):
			rest = tail
		default:
			return token
		}
	}
	return ""
}

// isEnvAssignmentToken reports whether a token is a POSIX `NAME=value` env
// prefix rather than a program. The name must be a plain identifier, so a path
// that merely contains an `=` (or a flag like `--out=x`) is still a program.
func isEnvAssignmentToken(token string) bool {
	name, _, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}

// nextScriptToken returns the first whitespace-delimited token of s with one
// pair of outer quotes removed, plus the remainder after it.
func nextScriptToken(s string) (token, rest string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i >= len(s) {
		return "", ""
	}
	start := i
	var quote byte
	for ; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == ' ' || c == '\t' {
			break
		}
	}
	token = s[start:i]
	if len(token) >= 2 && (token[0] == '\'' || token[0] == '"') && token[len(token)-1] == token[0] {
		token = token[1 : len(token)-1]
	}
	return token, s[i:]
}
