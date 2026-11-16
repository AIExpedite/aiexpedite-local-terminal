// File: session_owner.go
// -----------------------------------------------------------------------------
// The ownership marker every session process carries in its environment.
//
// beginSessionSpawn stamps AIEXPEDITE_SESSION_OWNER=<agent boot>:<session> on
// the CLI it is about to start; children inherit their parent's environment,
// so everything the session starts carries it too. On macOS and Linux it is the
// PROOF that a process is ours before the agent ends what a session left
// behind: a process group id says nothing on its own once the group has been
// empty, because the kernel may reuse it for an unrelated program
// (endLeftoverProcessTree, process_ownership_unix.go). A process that clears
// its environment loses the marker and is therefore never ended by that path:
// the cleanup fails safe. Windows proves ownership with the session's Job
// Object instead and does not read the marker.
// -----------------------------------------------------------------------------

package main

import (
	"os"
	"os/exec"
	"strings"
)

// sessionOwnerEnvVar names the marker variable.
const sessionOwnerEnvVar = "AIEXPEDITE_SESSION_OWNER"

// sessionOwnerValue is the marker of one session in one agent boot.
func sessionOwnerValue(bootID, sessionID string) string {
	return bootID + ":" + sessionID
}

// stampSessionOwner sets the marker on cmd's environment, replacing any value
// it inherited (a CLI started from inside another session must carry its OWN
// session's marker). A nil Env means "inherit the agent's", so it starts from
// os.Environ().
func stampSessionOwner(cmd *exec.Cmd, owner string) {
	if cmd == nil || owner == "" {
		return
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	prefix := sessionOwnerEnvVar + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	cmd.Env = append(out, prefix+owner)
}

// envHasSessionOwner reports whether a process environment carries exactly
// this owner marker.
func envHasSessionOwner(env []string, owner string) bool {
	want := sessionOwnerEnvVar + "=" + owner
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
