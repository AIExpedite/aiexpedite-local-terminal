// File: acp_fence.go
// -----------------------------------------------------------------------------
// The client-frame fence of the ACP family's OpenCode and Antigravity drivers
// (L3/L4 spec §2.3): every frame a SEND carries is checked BEFORE it reaches
// the CLI, and a frame outside the fence is refused with a typed
// `<kind>_acp_error` (errorCode ACP_FRAME_REFUSED) and never written.
//
// terminal-service builds every frame these sessions get (the handshake, the
// `session/prompt` it wraps a person's text in, the answers to the CLI's own
// requests), so the fence never gets in its way. It exists so that nothing
// else can: a frame that changes where the session works, what it may reach,
// whether it asks before acting, or that answers a question the CLI never
// asked.
//
//   - Requests: only the methods in acpFamilyClientRequests
//     (shared-constants ACP_CLIENT_METHOD_ALLOWLIST, the opencodeAcp and
//     antigravityAcp entries, which are identical). Never `session/fork`,
//     `session/delete`, `session/set_mode`, `session/list`, `authenticate`,
//     or an extension method: OpenCode's `_session/steering` is written by
//     the device's follow-up ledger only, never by a SEND.
//   - The one notification: `session/cancel`.
//   - `session/set_config_option`: only `model`, `effort`, `thought_level`.
//     Never `mode` — agy's `accept-edits` / `auto` modes stop it asking.
//   - Session setup (`session/new|load|resume`): `params.cwd` inside the
//     session's own start folder (resolveCwdForContainment, the check Grok's
//     setup frames get).
//   - Any frame: a non-empty `params.additionalDirectories` (it would widen
//     the folders the CLI may touch) and a non-empty `params.mcpServers` (an
//     ACP client hands the agent MCP servers to SPAWN; terminal-service
//     always sends none) are refused.
//   - Responses: only to an agent-to-client request that is still open on
//     this child. An answer to `session/request_permission` may select only
//     an option whose kind is `allow_once` or `reject_once` (or cancel): AI
//     Expedite never saves a standing rule in the person's CLI.
//   - Batches and scalars are refused by Send itself, before the fence.
//
// The fence reads the envelope of the frames it sees and the few params
// named above, never prompt text.
//
// Muse Code's MSP driver (`muse serve`) has a fence of its own, chosen by the
// profile's protocol: msp_fence.go.
// -----------------------------------------------------------------------------

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
)

// Wire values mirrored from shared-constants 3.23.579 (acpFamily.js
// ACP_CLIENT_METHOD_ALLOWLIST.opencodeAcp / .antigravityAcp, ACP_CONFIG_OPTION_IDS,
// ACP_START_ERROR_CODES). Contract-pinned by acp_fence_test.go.
var (
	acpFamilyClientRequests = []string{
		"initialize",
		"session/new",
		"session/load",
		"session/resume",
		"session/prompt",
		"session/set_config_option",
		"session/close",
	}
	acpFamilyClientNotifications   = []string{"session/cancel"}
	acpFamilyClientConfigOptionIDs = []string{"model", "effort", "thought_level"}
)

const (
	// ACP_START_ERROR_CODES.
	acpErrorCodeCLITooOld            = "ACP_CLI_TOO_OLD"
	acpErrorCodeConfigOverlayInvalid = "ACP_CONFIG_OVERLAY_INVALID"
	acpErrorCodeFrameRefused         = "ACP_FRAME_REFUSED"

	// ACP methods the fence reads beyond the tracker's.
	acpMethodSessionResume = "session/resume"
	acpMethodSetConfig     = "session/set_config_option"

	// ACP permission option kinds an answer may select.
	acpPermissionKindAllowOnce  = "allow_once"
	acpPermissionKindRejectOnce = "reject_once"

	// acpFenceOpenRequestsMax bounds the open agent-to-client requests one
	// child keeps. An ACP agent asks one question at a time in practice; the
	// cap only stops a runaway child growing the map. The oldest is dropped,
	// so the worst a flood can do is have one late answer refused.
	acpFenceOpenRequestsMax = 256
)

// acpClientFence is one OpenCode, Antigravity or Muse Code (MSP) child's
// fence.
type acpClientFence struct {
	noun string
	// msp is the MSP fence's own state (msp_fence.go); nil on an ACP child.
	msp *mspFenceState

	mu sync.Mutex
	// open holds the agent-to-client requests not yet answered, by id key
	// (jsonRPCIDKey: the number 7 and the string "7" are different requests).
	open  map[string]acpOpenRequest
	order []string
}

type acpOpenRequest struct {
	method string
	// options maps a permission request's optionId to its kind.
	options map[string]string
}

// acpFenceCheck is what a passed check knows that the write's outcome
// completes: the request a response answers, and (MSP) what admitting the
// frame reserves.
type acpFenceCheck struct {
	responseKey string

	mspStep   mspFenceStep
	mspCanon  string
	mspKey    string
	mspMethod string
}

func newACPClientFence(p *acpAgentProfile) *acpClientFence {
	f := &acpClientFence{noun: p.noun, open: map[string]acpOpenRequest{}}
	if p.protocol == acpProtocolMSP {
		f.msp = newMSPFenceState()
	}
	return f
}

func (f *acpClientFence) refuse(format string, args ...any) error {
	return &acpStartError{
		Code:    acpErrorCodeFrameRefused,
		Message: fmt.Sprintf("%s frame refused: %s", f.noun, fmt.Sprintf(format, args...)),
	}
}

// acpFenceParams are the params the fence reads.
type acpFenceParams struct {
	Cwd                   *string         `json:"cwd"`
	AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
	MCPServers            json.RawMessage `json:"mcpServers"`
	ConfigID              *string         `json:"configId"`
}

// check decides whether frame may be written to the child. root is the
// session's symlink-resolved start folder ("" only on a test fixture).
func (f *acpClientFence) check(frame, root string) (acpFenceCheck, error) {
	if f == nil {
		return acpFenceCheck{}, nil
	}
	if f.msp != nil {
		return f.checkMSP(frame, root)
	}
	rpc, ok := parseJSONRPCFrame(frame)
	if !ok {
		return acpFenceCheck{}, f.refuse("not a JSON-RPC object")
	}
	if rpc.Method == "" {
		if !rpc.isResponse() {
			return acpFenceCheck{}, f.refuse("neither a request, a notification nor a response")
		}
		return f.checkResponse(rpc)
	}
	allowed := acpFamilyClientNotifications
	kind := "notification"
	if rpc.hasID() {
		allowed, kind = acpFamilyClientRequests, "request"
	}
	if !containsString(allowed, rpc.Method) {
		return acpFenceCheck{}, f.refuse("%s %q is not one a client may send", kind, rpc.Method)
	}
	var params acpFenceParams
	if len(rpc.Params) > 0 && string(rpc.Params) != "null" {
		if err := json.Unmarshal(rpc.Params, &params); err != nil {
			return acpFenceCheck{}, f.refuse("%s params are not an object", rpc.Method)
		}
	}
	if jsonArrayNonEmpty(params.AdditionalDirectories) {
		return acpFenceCheck{}, f.refuse("%s names additionalDirectories", rpc.Method)
	}
	if jsonArrayNonEmpty(params.MCPServers) {
		return acpFenceCheck{}, f.refuse("%s names mcpServers", rpc.Method)
	}
	switch rpc.Method {
	case acpMethodSessionNew, acpMethodSessionLoad, acpMethodSessionResume:
		if params.Cwd != nil && root != "" {
			if err := checkACPSetupCwd(rpc.Method, *params.Cwd, root); err != nil {
				return acpFenceCheck{}, f.refuse("%v", err)
			}
		}
	case acpMethodSetConfig:
		if params.ConfigID == nil || !containsString(acpFamilyClientConfigOptionIDs, *params.ConfigID) {
			id := ""
			if params.ConfigID != nil {
				id = *params.ConfigID
			}
			return acpFenceCheck{}, f.refuse("%s may not set %q", rpc.Method, id)
		}
	}
	return acpFenceCheck{}, nil
}

// checkResponse admits an answer only to a request this child asked and has
// not had answered, and a permission answer only with a one-time option.
func (f *acpClientFence) checkResponse(rpc jsonRPCFrame) (acpFenceCheck, error) {
	key := rpc.idKey()
	f.mu.Lock()
	req, open := f.open[key]
	f.mu.Unlock()
	if !open {
		return acpFenceCheck{}, f.refuse("a response to id %s, which is not an open request", key)
	}
	if req.method == acpMethodRequestPermission && len(rpc.Result) > 0 {
		var answer struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		if err := json.Unmarshal(rpc.Result, &answer); err != nil {
			return acpFenceCheck{}, f.refuse("a permission answer that is not an outcome")
		}
		switch answer.Outcome.Outcome {
		case "cancelled":
		case "selected":
			kind, known := req.options[answer.Outcome.OptionID]
			if !known {
				return acpFenceCheck{}, f.refuse("a permission answer selecting an option the request did not offer")
			}
			if kind != acpPermissionKindAllowOnce && kind != acpPermissionKindRejectOnce {
				return acpFenceCheck{}, f.refuse("a permission answer selecting a %q option (only allow_once or reject_once)", kind)
			}
		default:
			return acpFenceCheck{}, f.refuse("a permission answer with outcome %q", answer.Outcome.Outcome)
		}
	}
	return acpFenceCheck{responseKey: key}, nil
}

// checkACPSetupCwd requires a session-setup frame's cwd to resolve inside
// root, the session's own start folder.
func checkACPSetupCwd(method, cwd, root string) error {
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("%s params.cwd must be an absolute path; got %q", method, cwd)
	}
	resolved, err := resolveCwdForContainment(cwd)
	if err != nil {
		return fmt.Errorf("%s params.cwd %q could not be safely resolved: %w", method, cwd, err)
	}
	if !resolvedPathWithin(root, resolved) {
		return fmt.Errorf("%s params.cwd %q is outside the session's workspace root %q (the directory the session was started in)", method, resolved, root)
	}
	return nil
}

// admit re-decides, under the session's stdin lock right before the write,
// what a concurrent write could have changed since check, and reserves it.
// Only the MSP fence has anything to reserve.
func (f *acpClientFence) admit(c acpFenceCheck) error {
	if f == nil || f.msp == nil {
		return nil
	}
	return f.admitMSP(c)
}

// written records that a checked frame reached the child: a response closes
// the request it answered.
func (f *acpClientFence) written(c acpFenceCheck) {
	if f == nil {
		return
	}
	if f.msp != nil {
		f.settleMSP(c, true)
		return
	}
	if c.responseKey == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.open, c.responseKey)
}

// release undoes what admit reserved for a frame that never reached the child.
func (f *acpClientFence) release(c acpFenceCheck) {
	if f == nil || f.msp == nil {
		return
	}
	f.settleMSP(c, false)
}

// inbound reads one frame off the child's stdout: a request it makes of the
// client is open until a response to it is written.
func (f *acpClientFence) inbound(line string) {
	_ = f.inboundFrame(line, "")
}

// inboundFrame is inbound with the session's start folder, for the MSP fence's
// check of a resumed conversation's folder. A non-nil error means the frame
// must not be published and the child must be ended (acp_family.go).
func (f *acpClientFence) inboundFrame(line, root string) error {
	if f == nil {
		return nil
	}
	if f.msp != nil {
		return f.inboundMSP(line, root)
	}
	rpc, ok := parseJSONRPCFrame(line)
	if !ok || !rpc.isRequest() {
		return nil
	}
	req := acpOpenRequest{method: rpc.Method}
	if rpc.Method == acpMethodRequestPermission {
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		if json.Unmarshal(rpc.Params, &p) == nil {
			req.options = make(map[string]string, len(p.Options))
			for _, o := range p.Options {
				if o.OptionID != "" {
					req.options[o.OptionID] = o.Kind
				}
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordOpenLocked(rpc.idKey(), req)
	return nil
}

// recordOpenLocked keeps an agent-to-client request open, within the cap.
// Caller holds f.mu.
func (f *acpClientFence) recordOpenLocked(key string, req acpOpenRequest) {
	if _, seen := f.open[key]; !seen {
		f.order = append(f.order, key)
	}
	f.open[key] = req
	// order also holds keys already answered: past the cap it is compacted to
	// the open ones (oldest first, each once), then the oldest open requests
	// go until the cap holds.
	if len(f.order) > acpFenceOpenRequestsMax {
		kept := make([]string, 0, len(f.open))
		listed := make(map[string]bool, len(f.open))
		for _, k := range f.order {
			if _, open := f.open[k]; open && !listed[k] {
				listed[k] = true
				kept = append(kept, k)
			}
		}
		f.order = kept
	}
	for len(f.open) > acpFenceOpenRequestsMax && len(f.order) > 0 {
		delete(f.open, f.order[0])
		f.order = f.order[1:]
	}
}

// jsonArrayNonEmpty reports a present value that is anything but an empty
// array or null: an object, a string or a non-empty array all count, so a
// shape the fence does not expect is refused rather than let through.
func jsonArrayNonEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return len(arr) > 0
	}
	return true
}
