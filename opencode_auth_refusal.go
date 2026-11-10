// opencode_auth_refusal.go — OpenCode's "no credentials for this provider"
// record, recognised in a direct-chat turn's stream and reported in its error.
//
// An `opencode run --format json` whose model belongs to a provider with no key
// on this computer exits 1 before calling a model, and its whole output is one
// top-level record (verified in prod, 2026-10-15/16):
//
//	{"type":"error","timestamp":…,"sessionID":"ses_…","error":{"name":
//	"ProviderAuthError","data":{"providerID":"google","message":"…"}}}
//
// The stream forwards it like any other event, but the turn's error frame said
// only "OpenCode exited with code 1 and produced no response", so nothing told
// voice or the chat which provider lacked a key — or that a key was the
// problem at all. A turn that saw the record and exits non-zero now fails with
//
//	OpenCode could not use its model: no credentials for provider <id> on this computer.
//	{"type":"error","error":{"name":"ProviderAuthError","data":{"providerID":"<id>"}}}
//
// a sentence a person can read, then the record rebuilt from the provider id
// alone as the error's LAST line — the line shared-constants
// openCodeAuthRefusalOnResult reads off a failed turn's output. `data.message`
// is never copied: it can name the user's home directory or the provider's key
// variable. The agent advertises `capabilities.openCodeAuthRefusalFrame`.
//
// The match is shared-constants openCodeAuthRefusal's, rule for rule: a line
// that starts with `{` and names ProviderAuthError, parsed as JSON, whose
// top-level `type` is "error" and `error.name` is "ProviderAuthError". The
// provider id counts only as a string of 1–40 word characters, dots or dashes;
// a record without a usable one is still a refusal, reported without a name.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

const (
	// openCodeAuthRecordType and openCodeAuthRecordErrorName mirror
	// shared-constants OPENCODE_AUTH_REQUIRED_SIGNALS (TYPE, ERROR_NAME).
	openCodeAuthRecordType      = "error"
	openCodeAuthRecordErrorName = "ProviderAuthError"
)

// openCodeAuthProviderIDPattern mirrors shared-constants OPENCODE_PROVIDER_ID
// (`^[\w.-]{1,40}$`; `\w` is ASCII in both engines).
var openCodeAuthProviderIDPattern = regexp.MustCompile(`^[\w.-]{1,40}$`)

// openCodeProviderAuthRefusal reports whether line is OpenCode's
// ProviderAuthError record, and the provider it names — "" when it names none
// this reader can vouch for. Only the type, the error's name and the provider
// id are decoded; nothing else in the record is read into a value.
func openCodeProviderAuthRefusal(line string) (providerID string, ok bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, openCodeAuthRecordErrorName) {
		return "", false
	}
	var record map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &record) != nil {
		return "", false
	}
	var recordType string
	if json.Unmarshal(record["type"], &recordType) != nil || recordType != openCodeAuthRecordType {
		return "", false
	}
	var errorFields map[string]json.RawMessage
	if json.Unmarshal(record["error"], &errorFields) != nil {
		return "", false
	}
	var name string
	if json.Unmarshal(errorFields["name"], &name) != nil || name != openCodeAuthRecordErrorName {
		return "", false
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(errorFields["data"], &data) == nil {
		var id string
		if json.Unmarshal(data["providerID"], &id) == nil && openCodeAuthProviderIDPattern.MatchString(id) {
			providerID = id
		}
	}
	return providerID, true
}

// openCodeAuthRefusalTurnError is the error a turn fails with when its stream
// carried the record: the sentence, a line break, and the record rebuilt from
// providerID alone. An id the pattern does not admit is dropped, never quoted.
func openCodeAuthRefusalTurnError(providerID string) string {
	if !openCodeAuthProviderIDPattern.MatchString(providerID) {
		providerID = ""
	}
	sentence := "OpenCode could not use its model: no credentials for its provider on this computer."
	if providerID != "" {
		sentence = "OpenCode could not use its model: no credentials for provider " + providerID + " on this computer."
	}
	return sentence + "\n" + openCodeAuthRefusalRecord(providerID)
}

// openCodeAuthRefusalRecord is the sanitized record: the type, the error's name
// and, when known, the provider id — nothing else.
func openCodeAuthRefusalRecord(providerID string) string {
	type recordData struct {
		ProviderID string `json:"providerID"`
	}
	type recordError struct {
		Name string      `json:"name"`
		Data *recordData `json:"data,omitempty"`
	}
	record := struct {
		Type  string      `json:"type"`
		Error recordError `json:"error"`
	}{Type: openCodeAuthRecordType, Error: recordError{Name: openCodeAuthRecordErrorName}}
	if providerID != "" {
		record.Error.Data = &recordData{ProviderID: providerID}
	}
	b, err := json.Marshal(record)
	if err != nil {
		// Unreachable for these types; still never an empty record.
		return `{"type":"error","error":{"name":"ProviderAuthError"}}`
	}
	return string(b)
}
