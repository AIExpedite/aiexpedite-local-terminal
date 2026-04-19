// Tests for the attestation verification path. The verifier is a small
// piece of code, but it sits on the critical path of the auto-update — a
// regression here either bricks updates entirely or silently disables the
// security check we just added. The tests use httptest to stand in for
// api.github.com so we don't depend on network state.
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifyBuildProvenance_RejectsInvalidHex(t *testing.T) {
	// Wrong length, non-hex chars — should reject before making any HTTP call.
	cases := []string{
		"",         // empty
		"deadbeef", // too short
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdead", // too long
		"x" + strings.Repeat("a", 63),                                          // non-hex char
		"ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",     // all non-hex
	}
	for _, badHex := range cases {
		err := verifyBuildProvenance("/tmp/ignored", badHex)
		if err == nil {
			t.Errorf("verifyBuildProvenance accepted bad hex %q", badHex)
		}
	}
}

// withMockedAttestationAPI swaps the attestation client's transport for a
// httptest server, letting us drive the verifier with controlled API
// responses. Restores the original transport on cleanup.
func withMockedAttestationAPI(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	originalTransport := attestationVerifyClient.Transport
	attestationVerifyClient.Transport = &rewriteTransport{server.URL}
	t.Cleanup(func() { attestationVerifyClient.Transport = originalTransport })
}

// rewriteTransport rewrites the request URL to point at the test server,
// preserving the original path so the handler can assert on it.
type rewriteTransport struct{ baseURL string }

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Construct a new request to the test server with the original path.
	newURL := rt.baseURL + req.URL.Path
	newReq, err := http.NewRequest(req.Method, newURL, req.Body)
	if err != nil {
		return nil, err
	}
	for k, v := range req.Header {
		newReq.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(newReq)
}

func TestVerifyBuildProvenance_NoAttestation(t *testing.T) {
	// 404 from the API → must fail (binary not built by our workflow, or
	// release predates the attest rollout — either way, refuse the update).
	withMockedAttestationAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	validHex := strings.Repeat("a", 64)
	err := verifyBuildProvenance("/tmp/ignored", validHex)
	if err == nil {
		t.Fatal("expected error when API returns 404, got nil")
	}
	if !strings.Contains(err.Error(), "no attestation") {
		t.Errorf("error message should mention missing attestation, got: %v", err)
	}
}

func TestVerifyBuildProvenance_EmptyAttestationList(t *testing.T) {
	// 200 with empty list → must fail. This is the case when GitHub has the
	// repo registered but no attestations exist for the digest yet.
	withMockedAttestationAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"attestations":[]}`)
	})

	validHex := strings.Repeat("a", 64)
	err := verifyBuildProvenance("/tmp/ignored", validHex)
	if err == nil {
		t.Fatal("expected error for empty attestations list")
	}
}

func TestVerifyBuildProvenance_RepoIDMismatch(t *testing.T) {
	// API returns an attestation for the WRONG repo ID. Could happen via:
	//   - URL/path injection (we already block this with hex validation)
	//   - GitHub API change broadening response scope
	//   - A repo transfer where stale attestations remain
	// Either way, refuse the update.
	withMockedAttestationAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"attestations":[{"repository_id":99999999,"bundle_url":"x","initiator":"user"}]}`)
	})

	validHex := strings.Repeat("a", 64)
	err := verifyBuildProvenance("/tmp/ignored", validHex)
	if err == nil {
		t.Fatal("expected error for repo_id mismatch")
	}
	if !strings.Contains(err.Error(), "repo_id mismatch") {
		t.Errorf("error should mention repo_id mismatch, got: %v", err)
	}
}

func TestVerifyBuildProvenance_Success(t *testing.T) {
	// Happy path: API returns an attestation with the expected repo ID.
	withMockedAttestationAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"attestations":[{"repository_id":%d,"bundle_url":"https://example/bundle","initiator":"user"}]}`, expectedRepoID)
	})

	validHex := strings.Repeat("a", 64)
	if err := verifyBuildProvenance("/tmp/ignored", validHex); err != nil {
		t.Errorf("expected success, got error: %v", err)
	}
}

func TestVerifyBuildProvenance_EnvVarBypass(t *testing.T) {
	// The escape-hatch env var should return errAttestationDisabled (a
	// distinct sentinel), so the caller can log differently and proceed.
	t.Setenv("AIEXPEDITE_SKIP_ATTESTATION_VERIFY", "1")

	// No mock needed — bypass should short-circuit before any HTTP call.
	err := verifyBuildProvenance("/tmp/ignored", strings.Repeat("a", 64))
	if err == nil {
		t.Fatal("expected errAttestationDisabled, got nil")
	}
	// Caller distinguishes via errors.Is — verify the sentinel is returned.
	if err.Error() != errAttestationDisabled.Error() {
		t.Errorf("expected errAttestationDisabled, got: %v", err)
	}
}
