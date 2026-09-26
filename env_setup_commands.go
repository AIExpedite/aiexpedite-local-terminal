// File: env_setup_commands.go
// -----------------------------------------------------------------------------
// The computer setup checklist's device commands (setup checklist build
// contract §3; COMPUTER_SETUP_CHECKLIST_PLAN.md §7 steps 2, 4 and 6):
//
//	__env_find_repos__   read_only       list git checkouts directly under roots
//	__env_verify_deps__  read_only       prove node_modules matches package-lock.json
//	__env_sign_in__      external_write  open a visible terminal running a sign-in
//
// They arrive as ordinary env-setup `execute` dispatches (signed command + args
// + riskLevel + cwd) and pass the same gates as every other step — signature,
// staleness, per-UID rate limit, update-drain admission. They are intercepted
// BEFORE exec: the command name is never looked up on PATH, and nothing is
// handed to a shell. Only the approval gate differs (see
// envSetupCommandNeedsApproval): the allow list governs raw command lines, which
// these are not, so find / verify skip it like the other built-in commands,
// while a sign-in ALWAYS takes the native approval dialog that external_write
// steps take today — at external_write even if the signed riskLevel says less.
//
// Wire format: args[0] is a JSON string. The result goes back on the normal
// command-result path: status "success" with Output = one line of JSON, or
// status "error" with the error text.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	envFindReposCommand  = "__env_find_repos__"
	envVerifyDepsCommand = "__env_verify_deps__"
	envSignInCommand     = "__env_sign_in__"

	riskReadOnly      = "read_only"
	riskExternalWrite = "external_write"
	riskDestructive   = "destructive"

	// maxEnvSetupCommandArgBytes bounds args[0]; the largest real payload (two
	// roots, or a sign-in argv) is a few hundred bytes.
	maxEnvSetupCommandArgBytes = 16 * 1024
)

// isEnvSetupDeviceCommand reports whether command is one of the setup
// checklist's built-in device commands.
func isEnvSetupDeviceCommand(command string) bool {
	switch command {
	case envFindReposCommand, envVerifyDepsCommand, envSignInCommand:
		return true
	}
	return false
}

// effectiveEnvSetupRisk is the risk a device command is gated at: the signed
// riskLevel, except that a sign-in is never below external_write — it launches
// a program that writes credentials, whatever the dispatch claims.
func effectiveEnvSetupRisk(cmd commandMsg) string {
	if cmd.Command == envSignInCommand && cmd.RiskLevel != riskDestructive {
		return riskExternalWrite
	}
	return cmd.RiskLevel
}

// envSetupCommandNeedsApproval reports whether a device command must show the
// native approval dialog: exactly when its effective risk is one that forces
// native approval for any other env-setup step (requiresNativeApprovalForStep).
func envSetupCommandNeedsApproval(cmd commandMsg) bool {
	gated := cmd
	gated.RiskLevel = effectiveEnvSetupRisk(cmd)
	return requiresNativeApprovalForStep(gated)
}

// envSetupApprovalDisplay is what the approval dialog shows for a device
// command: a sign-in shows the program and arguments it will run (not the JSON
// envelope); the read-only commands show their name.
func envSetupApprovalDisplay(cmd commandMsg) (string, []string) {
	if cmd.Command == envSignInCommand {
		if req, err := parseEnvSignInRequest(cmd.Args); err == nil {
			return req.Argv[0], req.Argv[1:]
		}
	}
	return cmd.Command, nil
}

// decodeEnvSetupArgs unmarshals args[0] into v. Unknown fields are ignored so a
// newer server can add optional inputs without breaking this agent.
func decodeEnvSetupArgs(args []string, v any) error {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return errors.New("missing JSON argument")
	}
	if len(args[0]) > maxEnvSetupCommandArgBytes {
		return fmt.Errorf("JSON argument exceeds %d bytes", maxEnvSetupCommandArgBytes)
	}
	if err := json.Unmarshal([]byte(args[0]), v); err != nil {
		return fmt.Errorf("invalid JSON argument: %w", err)
	}
	return nil
}

// runEnvSetupDeviceCommand runs one device command and returns its one-line
// JSON result. Every string in the result is passed through
// redactSensitiveData BEFORE encoding (redactJSONStrings), so the caller must
// publish it without the whole-output redaction: that pass's `\S+` patterns
// would run across JSON punctuation and could corrupt the document.
func runEnvSetupDeviceCommand(ctx context.Context, cfg *Config, cmd commandMsg) (string, error) {
	var result any
	var err error
	switch cmd.Command {
	case envFindReposCommand:
		var req envFindReposRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = findRepositoryCheckouts(ctx, req)
		}
	case envVerifyDepsCommand:
		var req envVerifyDepsRequest
		if err = decodeEnvSetupArgs(cmd.Args, &req); err == nil {
			result, err = verifyDependencies(ctx, req, currentNpmPlatform())
		}
	case envSignInCommand:
		var req envSignInRequest
		if req, err = parseEnvSignInRequest(cmd.Args); err == nil {
			result, err = launchSignIn(req)
		}
	default:
		err = fmt.Errorf("unknown setup command %q", cmd.Command)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %s", cmd.Command, redactSensitiveData(err.Error()))
	}
	return encodeRedactedJSON(result)
}

// encodeRedactedJSON encodes v as one line of JSON with every string value
// redacted individually.
func encodeRedactedJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return "", err
	}
	out, err := json.Marshal(redactJSONStrings(generic))
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// redactJSONStrings applies redactSensitiveData to every string value (and map
// key) of a decoded JSON document.
func redactJSONStrings(v any) any {
	switch t := v.(type) {
	case string:
		return redactSensitiveData(t)
	case []any:
		for i := range t {
			t[i] = redactJSONStrings(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[redactSensitiveData(k)] = redactJSONStrings(val)
		}
		return out
	default:
		return v
	}
}
