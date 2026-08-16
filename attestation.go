// File: attestation.go
//
// Verifies that a downloaded update binary has a valid SLSA build provenance
// attestation published by THIS repository's release workflow. Without this
// check, anyone who can land a release tag in AIExpedite/aiexpedite-local-terminal
// can ship arbitrary code to every install — having an attestation step in CI
// is decoration unless the client actually verifies it.
//
// Threat model & verification scope:
//
//   ✓ Defends against: a release tag pointing to a binary not produced by
//     our own GitHub Actions workflow (the most likely attack on any
//     auto-update flow). To forge an attestation, the attacker would need
//     to either compromise our workflow source (full repo write) or bypass
//     GitHub's sigstore validation (unprecedented).
//
//   ✓ Defends against: man-in-the-middle of the binary download. We re-verify
//     against api.github.com over a separate HTTPS connection.
//
//   ✗ Does NOT defend against: an attacker who can also push to .github/workflows/
//     in this repo. They could just modify the workflow to ship anything.
//     If you have repo write, the game is already over.
//
//   ✗ Does NOT defend against: compromise of api.github.com itself. Same
//     trust assumption as the binary download.
//
// Implementation note: this verifies attestation EXISTENCE via GitHub's
// Attestations API (which itself verifies the sigstore bundle at storage
// time). It does not currently re-verify the bundle's signature client-side
// or pin the predicate's workflow path. That's a deliberate tradeoff —
// full client-side sigstore verification would require pulling in
// sigstore-go (~10MB of crypto deps) or shelling out to `gh attestation
// verify` (UX problem if gh isn't installed). The "attestation exists for
// our SHA, in our repo" check covers the realistic threat at much lower
// cost.
//
// TODO(future): pin predicate.buildDefinition.externalParameters.workflow.path
// to one of our known release workflows. Requires fetching the bundle from
// bundle_url (Snappy-compressed) and parsing the DSSE envelope, OR migrating
// to sigstore-go for full bundle verification.

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// expectedRepoID is the numeric repo ID of AIExpedite/aiexpedite-local-terminal.
// We compare against the immutable ID rather than the name to defend against
// repo rename/transfer attacks (the API would otherwise return attestations
// from a renamed repo that an attacker could control).
const expectedRepoID = 973661411

// attestationVerifyClient has its own short timeout — verification is on the
// critical path of an update, so we don't want to hang the user.
var attestationVerifyClient = &http.Client{Timeout: 30 * time.Second}

// errAttestationDisabled is returned when the user has explicitly opted out
// (env var). Lets the update code distinguish "user said skip" from "verify
// failed" so we can log differently.
var errAttestationDisabled = errors.New("attestation verification disabled by env var")

// attestationListResponse mirrors the relevant fields of the
// `/repos/{owner}/{repo}/attestations/sha256:{digest}` response.
// We only inspect repository_id; the bundle/bundle_url payload is left
// opaque (TODO above).
type attestationListResponse struct {
	Attestations []struct {
		RepositoryID int64  `json:"repository_id"`
		BundleURL    string `json:"bundle_url"`
		Initiator    string `json:"initiator"`
	} `json:"attestations"`
}

// verifyBuildProvenanceWithContext checks that the GitHub Attestations API returns at
// least one valid attestation for the given binary's SHA-256 digest, scoped
// to our repository (by immutable repo ID), respecting ctx.
//
// Returns nil on success. Returns an error if no attestation is found, the
// API call fails, or the response references a different repo. The caller
// MUST refuse to apply the update if this returns a non-nil error (other
// than errAttestationDisabled).
//
// The `AIEXPEDITE_SKIP_ATTESTATION_VERIFY=1` env var bypasses this check —
// intended ONLY as an escape hatch for users behind firewalls that block
// api.github.com. Logged loudly when used.
func verifyBuildProvenanceWithContext(ctx context.Context, binaryPath string, sha256Hex string) error {
	if os.Getenv("AIEXPEDITE_SKIP_ATTESTATION_VERIFY") == "1" {
		LogSecurityEvent(SecEvtAttestationSkipped,
			"attestation verification bypassed via AIEXPEDITE_SKIP_ATTESTATION_VERIFY=1",
			"sha256", sha256Hex, "binary_path", binaryPath)
		return errAttestationDisabled
	}

	// SHA-256 hex must be exactly 64 lowercase chars; reject malformed input
	// before constructing a URL (defense against accidental injection into
	// the URL path).
	if len(sha256Hex) != 64 {
		return fmt.Errorf("invalid sha256 hex length: got %d, want 64", len(sha256Hex))
	}
	if _, err := hex.DecodeString(sha256Hex); err != nil {
		return fmt.Errorf("invalid sha256 hex: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/attestations/sha256:%s", githubRepo, sha256Hex)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := attestationVerifyClient.Do(req) //nolint:gosec // URL is repo-scoped + hex-validated
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		LogSecurityEvent(SecEvtAttestationFailed, "attestation API unreachable",
			"sha256", sha256Hex, "error", err.Error())
		return fmt.Errorf("fetch attestation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// 404 here means GitHub has no attestation for this digest —
		// either the binary wasn't built by our attest-build-provenance
		// workflow, or the binary was tampered with after build. Either
		// way: refuse the update.
		LogSecurityEvent(SecEvtAttestationFailed, "no attestation for binary digest",
			"sha256", sha256Hex, "http_status", 404)
		return fmt.Errorf("no attestation found for binary (sha256:%s) — release may be tampered or pre-attestation-rollout", sha256Hex)
	}
	if resp.StatusCode != http.StatusOK {
		LogSecurityEvent(SecEvtAttestationFailed, "unexpected attestation API status",
			"sha256", sha256Hex, "http_status", resp.StatusCode)
		return fmt.Errorf("attestation API returned HTTP %d", resp.StatusCode)
	}

	// Cap at 256KB — the response is a small JSON list, not bundle contents.
	var list attestationListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&list); err != nil {
		LogSecurityEvent(SecEvtAttestationFailed, "could not decode attestation response",
			"sha256", sha256Hex, "error", err.Error())
		return fmt.Errorf("decode attestation response: %w", err)
	}

	if len(list.Attestations) == 0 {
		LogSecurityEvent(SecEvtAttestationFailed, "empty attestation list",
			"sha256", sha256Hex)
		return fmt.Errorf("no attestations returned for sha256:%s", sha256Hex)
	}

	// Verify at least one attestation references the EXPECTED repo ID.
	// GitHub's API only returns attestations for the queried repo path,
	// but we double-check via repo ID to defend against:
	//   - URL-path injection (defense-in-depth — sha256Hex is already validated)
	//   - Future API changes that broaden the response scope
	for _, att := range list.Attestations {
		if att.RepositoryID == expectedRepoID {
			LogSecurityEvent(SecEvtAttestationVerified,
				"build provenance attestation verified",
				"sha256", sha256Hex, "repo_id", att.RepositoryID, "initiator", att.Initiator)
			return nil
		}
	}

	// Found attestations but none match our repo ID. Log the actual repo IDs
	// returned so a debug investigation can spot fork/transfer scenarios.
	var foundIDs []string
	for _, att := range list.Attestations {
		foundIDs = append(foundIDs, fmt.Sprintf("%d", att.RepositoryID))
	}
	LogSecurityEvent(SecEvtAttestationFailed, "repo_id mismatch in attestation",
		"sha256", sha256Hex, "expected_repo_id", expectedRepoID, "found_repo_ids", foundIDs)
	return fmt.Errorf("attestation found but repo_id mismatch (expected %d, got [%s])",
		expectedRepoID, strings.Join(foundIDs, ", "))
}

// verifyBuildProvenance checks that the GitHub Attestations API returns at
// least one valid attestation for the given binary's SHA-256 digest.
func verifyBuildProvenance(binaryPath string, sha256Hex string) error {
	return verifyBuildProvenanceWithContext(context.Background(), binaryPath, sha256Hex)
}
