// File: musecode_msp_surface.go
// -----------------------------------------------------------------------------
// The schema check behind `capabilities.mspDrivers` (L5 spec §2.5): before
// this computer reports Muse Code's MSP driver, the installed binary's OWN
// wire contract must have everything the driver uses.
//
// Muse publishes no SDK and no wire changelog, so the binary is the contract:
// `muse schema generate-json-schema --out DIR` writes the stable MSP bundle
// embedded in that build (`msp.schema.json`), offline and in well under a
// second, with no model call and nothing written outside DIR. The check reads
// it and requires shared-constants MUSE_MSP_REQUIRED_SURFACE (mirrored below):
// the methods the driver sends, the host request it answers, the
// notifications it reads, `turn/steer`'s required params, the chat approval
// mode, and the two error codes the follow-up classifier branches on.
//
// The verdict is cached by the binary's (path, mtime, size), like the version
// probe, so a steady state spawns nothing; a new build is checked once. A
// failure to RUN the export (spawn error, timeout, unreadable output) is not
// cached and reads as "not there": the driver is simply not reported, and the
// next gather or START tries again.
//
// Why a version floor too (musecode_msp_driver.go): this proves the surface
// EXISTS; the floor is the version whose BEHAVIOUR was checked on the wire.
// -----------------------------------------------------------------------------

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// museMSPSurface is the part of an MSP schema export the driver depends on,
// in the shape of shared-constants tests/fixtures/muse-msp-1.4.18.surface.json
// (the 1.4.18 export, summarised; a copy is testdata/muse-msp-1.4.18.surface.json).
type museMSPSurface struct {
	Methods                 []string             `json:"methods"`
	Notifications           []string             `json:"notifications"`
	ServerRequests          []string             `json:"serverRequests"`
	Errors                  []museMSPSurfaceCode `json:"errors"`
	TurnSteerParamsRequired []string             `json:"turnSteerParamsRequired"`
	ApprovalModes           []string             `json:"approvalModes"`
}

type museMSPSurfaceCode struct {
	Code int64 `json:"code"`
}

// museMSPRequiredSurface mirrors shared-constants 3.23.580
// MUSE_MSP_REQUIRED_SURFACE (museCode.js). Contract-pinned by
// musecode_msp_surface_test.go.
var museMSPRequiredSurface = struct {
	methods, serverRequests, notifications, turnSteerParamsRequired, approvalModes []string
	errorCodes                                                                     []int64
}{
	methods: []string{
		"initialize", "session/start", "session/resume", "session/setApprovalMode",
		"turn/start", "turn/steer", "turn/interrupt", "approval/decide", "usage/read",
	},
	serverRequests: []string{"approval/request"},
	notifications: []string{
		"turn/started", "turn/foregroundCompleted", "turn/completed",
		"item/started", "item/delta", "item/completed", "approval/resolved",
	},
	turnSteerParamsRequired: []string{"commandId", "expectedTurnId", "input", "sessionId"},
	approvalModes:           []string{museMSPChatApprovalMode},
	errorCodes:              []int64{mspErrSessionNotFound, mspErrCommandRejected},
}

// checkMuseMSPSurface is nil when s has every entry of the required surface,
// else an error naming the first one missing.
func checkMuseMSPSurface(s museMSPSurface) error {
	r := museMSPRequiredSurface
	for _, want := range []struct {
		what string
		need []string
		have []string
	}{
		{"method", r.methods, s.Methods},
		{"server request", r.serverRequests, s.ServerRequests},
		{"notification", r.notifications, s.Notifications},
		{"turn/steer required param", r.turnSteerParamsRequired, s.TurnSteerParamsRequired},
		{"approval mode", r.approvalModes, s.ApprovalModes},
	} {
		for _, name := range want.need {
			if !containsString(want.have, name) {
				return fmt.Errorf("the MSP schema has no %s %q", want.what, name)
			}
		}
	}
	for _, code := range r.errorCodes {
		found := false
		for _, e := range s.Errors {
			if e.Code == code {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("the MSP schema has no error code %d", code)
		}
	}
	return nil
}

// summarizeMuseMSPSchema reads an `msp.schema.json` export into its surface:
// the keys of `methods`, `notifications` and `requests`, the codes of
// `errors`, `$defs.TurnSteerParams.required` and `$defs.ApprovalMode.enum`.
func summarizeMuseMSPSchema(raw []byte) (museMSPSurface, error) {
	var doc struct {
		Methods       map[string]json.RawMessage `json:"methods"`
		Notifications map[string]json.RawMessage `json:"notifications"`
		Requests      map[string]json.RawMessage `json:"requests"`
		Errors        []museMSPSurfaceCode       `json:"errors"`
		Defs          struct {
			TurnSteerParams struct {
				Required []string `json:"required"`
			} `json:"TurnSteerParams"`
			ApprovalMode struct {
				Enum []string `json:"enum"`
			} `json:"ApprovalMode"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return museMSPSurface{}, fmt.Errorf("the MSP schema export is not JSON: %w", err)
	}
	if len(doc.Methods) == 0 {
		return museMSPSurface{}, fmt.Errorf("the MSP schema export lists no methods")
	}
	keys := func(m map[string]json.RawMessage) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	return museMSPSurface{
		Methods:                 keys(doc.Methods),
		Notifications:           keys(doc.Notifications),
		ServerRequests:          keys(doc.Requests),
		Errors:                  doc.Errors,
		TurnSteerParamsRequired: doc.Defs.TurnSteerParams.Required,
		ApprovalModes:           doc.Defs.ApprovalMode.Enum,
	}, nil
}

/* --------------------------------------------------------------------------
   The probe and its cache
   -------------------------------------------------------------------------- */

const (
	// museMSPSchemaExportTimeout bounds one export. It is offline and took
	// 80 ms on Muse 1.4.20; the slack is for a cold disk or antivirus scan.
	museMSPSchemaExportTimeout = 20 * time.Second
	// museMSPSchemaMaxBytes caps the export read (the 1.4.20 bundle is ~480 KB).
	museMSPSchemaMaxBytes = 32 << 20
	// museMSPSchemaFileName is the bundle the export writes beside manifest.json.
	museMSPSchemaFileName = "msp.schema.json"
)

var (
	// museMSPSurfaceMu serialises probes and guards the cache: a START and a
	// capability gather asking about the same new build run one export.
	museMSPSurfaceMu    sync.Mutex
	museMSPSurfaceCache = map[versionProbeKey]bool{}

	// exportMuseMSPSchema runs the binary's schema export and returns
	// msp.schema.json. A var so tests never spawn a real Muse.
	exportMuseMSPSchema = runMuseMSPSchemaExport
)

// museCodeMSPSurfaceOK reports whether binary's own schema export has the MSP
// surface the driver needs, cached by the binary's identity.
func museCodeMSPSurfaceOK(binary string) bool {
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	key := versionProbeKey{Path: binary, ModUnix: info.ModTime().UnixNano(), Size: info.Size()}
	museMSPSurfaceMu.Lock()
	defer museMSPSurfaceMu.Unlock()
	if ok, cached := museMSPSurfaceCache[key]; cached {
		return ok
	}
	raw, err := exportMuseMSPSchema(binary)
	if err != nil {
		fmt.Printf("%s[musecode-msp] could not read the MSP schema of %s: %v%s\n", colorYellow, binary, err, colorReset)
		return false
	}
	ok := false
	if s, err := summarizeMuseMSPSchema(raw); err != nil {
		fmt.Printf("%s[musecode-msp] %s: %v%s\n", colorYellow, binary, err, colorReset)
	} else if err := checkMuseMSPSurface(s); err != nil {
		fmt.Printf("%s[musecode-msp] %s cannot run on the MSP driver: %v%s\n", colorYellow, binary, err, colorReset)
	} else {
		ok = true
	}
	// One verdict per path: an upgraded build replaces the old one's.
	for k := range museMSPSurfaceCache {
		if k.Path == key.Path {
			delete(museMSPSurfaceCache, k)
		}
	}
	museMSPSurfaceCache[key] = ok
	return ok
}

// runMuseMSPSchemaExport runs `<binary> schema generate-json-schema --out
// <private temp dir>` and reads the bundle back. The temp dir is removed.
func runMuseMSPSchemaExport(binary string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "aix-muse-schema-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "out")
	ctx, cancel := context.WithTimeout(context.Background(), museMSPSchemaExportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "schema", "generate-json-schema", "--out", out)
	cmd.Env = stripEnvPrefixes(os.Environ(), museCodeUnrelatedStripped)
	cmd.Dir = dir
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	hideWindow(cmd)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("schema export: %w", err)
	}
	path := filepath.Join(out, museMSPSchemaFileName)
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > museMSPSchemaMaxBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d-byte cap", museMSPSchemaFileName, info.Size(), museMSPSchemaMaxBytes)
	}
	return os.ReadFile(path)
}
