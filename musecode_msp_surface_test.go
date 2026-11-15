package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// loadMuseMSPSurfaceFixture is testdata/muse-msp-1.4.18.surface.json, a byte
// for byte copy of shared-constants tests/fixtures/muse-msp-1.4.18.surface.json
// (the Muse Code 1.4.18 export, summarised).
func loadMuseMSPSurfaceFixture(t *testing.T) museMSPSurface {
	t.Helper()
	raw, err := os.ReadFile("testdata/muse-msp-1.4.18.surface.json")
	if err != nil {
		t.Fatal(err)
	}
	var s museMSPSurface
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Methods) == 0 || len(s.Errors) == 0 {
		t.Fatalf("fixture did not parse: %+v", s)
	}
	return s
}

// The surface the device requires is shared-constants 3.23.580's
// MUSE_MSP_REQUIRED_SURFACE, value for value.
func TestMuseMSPRequiredSurfaceMatchesSharedConstants(t *testing.T) {
	r := museMSPRequiredSurface
	want := map[string][]string{
		"methods": {"initialize", "session/start", "session/resume", "session/setApprovalMode",
			"turn/start", "turn/steer", "turn/interrupt", "approval/decide", "usage/read"},
		"serverRequests": {"approval/request"},
		"notifications": {"turn/started", "turn/foregroundCompleted", "turn/completed",
			"item/started", "item/delta", "item/completed", "approval/resolved"},
		"turnSteerParamsRequired": {"commandId", "expectedTurnId", "input", "sessionId"},
		"approvalModes":           {"promptUnmatched"},
	}
	got := map[string][]string{
		"methods": r.methods, "serverRequests": r.serverRequests, "notifications": r.notifications,
		"turnSteerParamsRequired": r.turnSteerParamsRequired, "approvalModes": r.approvalModes,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("required surface = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(r.errorCodes, []int64{-32020, -32030}) {
		t.Fatalf("error codes = %v", r.errorCodes)
	}
}

func TestTheMuse1418SurfaceFixturePasses(t *testing.T) {
	if err := checkMuseMSPSurface(loadMuseMSPSurfaceFixture(t)); err != nil {
		t.Fatal(err)
	}
}

func without(list []string, drop string) []string {
	var out []string
	for _, v := range list {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

// A build missing any part of the surface is never reported: turn/steer,
// TurnSteerParams' expectedTurnId, the chat approval mode, approval requests,
// the foreground fence's notification, a classified error code.
func TestASchemaMissingAnyPartOfTheSurfaceFails(t *testing.T) {
	cases := map[string]func(s *museMSPSurface){
		"no turn/steer":      func(s *museMSPSurface) { s.Methods = without(s.Methods, "turn/steer") },
		"no approval/decide": func(s *museMSPSurface) { s.Methods = without(s.Methods, "approval/decide") },
		"steer without expectedTurnId": func(s *museMSPSurface) {
			s.TurnSteerParamsRequired = without(s.TurnSteerParamsRequired, "expectedTurnId")
		},
		"no promptUnmatched":             func(s *museMSPSurface) { s.ApprovalModes = without(s.ApprovalModes, "promptUnmatched") },
		"no approval/request":            func(s *museMSPSurface) { s.ServerRequests = without(s.ServerRequests, "approval/request") },
		"no turn/foregroundCompleted":    func(s *museMSPSurface) { s.Notifications = without(s.Notifications, "turn/foregroundCompleted") },
		"no commandRejected error code":  func(s *museMSPSurface) { s.Errors = withoutCode(s.Errors, -32030) },
		"no sessionNotFound error code":  func(s *museMSPSurface) { s.Errors = withoutCode(s.Errors, -32020) },
		"an empty export (no anything)":  func(s *museMSPSurface) { *s = museMSPSurface{} },
		"approval/resolved not notified": func(s *museMSPSurface) { s.Notifications = without(s.Notifications, "approval/resolved") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := loadMuseMSPSurfaceFixture(t)
			mutate(&s)
			if err := checkMuseMSPSurface(s); err == nil {
				t.Fatal("a surface missing part of what the driver uses passed")
			}
		})
	}
}

func withoutCode(list []museMSPSurfaceCode, drop int64) []museMSPSurfaceCode {
	var out []museMSPSurfaceCode
	for _, e := range list {
		if e.Code != drop {
			out = append(out, e)
		}
	}
	return out
}

// rawMuseMSPSchema renders a surface in the shape `muse schema
// generate-json-schema` writes msp.schema.json (methods / notifications /
// requests as objects keyed by name, errors as a list, the two $defs).
func rawMuseMSPSchema(s museMSPSurface) []byte {
	obj := func(names []string) map[string]any {
		m := map[string]any{}
		for _, n := range names {
			m[n] = map[string]any{"description": n}
		}
		return m
	}
	errs := []map[string]any{}
	for _, e := range s.Errors {
		errs = append(errs, map[string]any{"code": e.Code, "kind": "k", "retryable": false})
	}
	b, _ := json.Marshal(map[string]any{
		"$schema":       "https://json-schema.org/draft/2020-12/schema",
		"methods":       obj(s.Methods),
		"notifications": obj(s.Notifications),
		"requests":      obj(s.ServerRequests),
		"errors":        errs,
		"$defs": map[string]any{
			"TurnSteerParams": map[string]any{"type": "object", "required": s.TurnSteerParamsRequired},
			"ApprovalMode":    map[string]any{"type": "string", "enum": s.ApprovalModes},
		},
	})
	return b
}

func TestSummarizeMuseMSPSchemaReadsTheExportShape(t *testing.T) {
	fixture := loadMuseMSPSurfaceFixture(t)
	got, err := summarizeMuseMSPSchema(rawMuseMSPSchema(fixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range [][]string{got.Methods, got.Notifications, got.ServerRequests, fixture.Methods, fixture.Notifications, fixture.ServerRequests} {
		sort.Strings(l)
	}
	if !reflect.DeepEqual(got.Methods, fixture.Methods) || !reflect.DeepEqual(got.Notifications, fixture.Notifications) ||
		!reflect.DeepEqual(got.ServerRequests, fixture.ServerRequests) ||
		!reflect.DeepEqual(got.TurnSteerParamsRequired, fixture.TurnSteerParamsRequired) ||
		!reflect.DeepEqual(got.ApprovalModes, fixture.ApprovalModes) || len(got.Errors) != len(fixture.Errors) {
		t.Fatalf("summary does not round-trip the fixture: %+v", got)
	}
	if err := checkMuseMSPSurface(got); err != nil {
		t.Fatal(err)
	}
	if _, err := summarizeMuseMSPSchema([]byte(`{"methods":{}}`)); err == nil {
		t.Fatal("an export listing no methods was read as a schema")
	}
	if _, err := summarizeMuseMSPSchema([]byte(`not json`)); err == nil {
		t.Fatal("garbage was read as a schema")
	}
}

// pinMuseMSPSchemaExport makes the schema export answer from export for one
// test, and clears the verdict cache around it.
func pinMuseMSPSchemaExport(t *testing.T, export func(binary string) ([]byte, error)) {
	t.Helper()
	museMSPSurfaceMu.Lock()
	prev := exportMuseMSPSchema
	exportMuseMSPSchema = export
	museMSPSurfaceCache = map[versionProbeKey]bool{}
	museMSPSurfaceMu.Unlock()
	t.Cleanup(func() {
		museMSPSurfaceMu.Lock()
		exportMuseMSPSchema = prev
		museMSPSurfaceCache = map[versionProbeKey]bool{}
		museMSPSurfaceMu.Unlock()
	})
}

// The verdict is cached by the binary's identity: a steady state spawns
// nothing, a new build is checked once, and a failure to RUN the export is
// not remembered.
func TestMuseCodeMSPSurfaceVerdictIsCachedByBinaryIdentity(t *testing.T) {
	good := rawMuseMSPSchema(loadMuseMSPSurfaceFixture(t))
	runs := 0
	fail := false
	pinMuseMSPSchemaExport(t, func(string) ([]byte, error) {
		runs++
		if fail {
			return nil, errors.New("timed out")
		}
		return good, nil
	})
	bin := filepath.Join(t.TempDir(), "muse-bin-1.4.20-R1.exe")
	if err := os.WriteFile(bin, []byte("v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !museCodeMSPSurfaceOK(bin) || !museCodeMSPSurfaceOK(bin) || runs != 1 {
		t.Fatalf("an unchanged build was exported %d times", runs)
	}
	// A new build (another size) is checked again.
	if err := os.WriteFile(bin, []byte("v2-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(bin, future, future)
	fail = true
	if museCodeMSPSurfaceOK(bin) || museCodeMSPSurfaceOK(bin) || runs != 3 {
		t.Fatalf("a failed export was cached or passed (%d runs)", runs)
	}
	fail = false
	if !museCodeMSPSurfaceOK(bin) || runs != 4 {
		t.Fatalf("the build was not checked again after a failed run (%d runs)", runs)
	}
	if museCodeMSPSurfaceOK(filepath.Join(t.TempDir(), "absent")) || runs != 4 {
		t.Fatal("a missing binary was exported")
	}
}

// A build whose export lacks the surface is cached as not qualifying.
func TestMuseCodeMSPSurfaceRefusesAnExportWithoutTheSurface(t *testing.T) {
	s := loadMuseMSPSurfaceFixture(t)
	s.Methods = without(s.Methods, "turn/steer")
	runs := 0
	pinMuseMSPSchemaExport(t, func(string) ([]byte, error) { runs++; return rawMuseMSPSchema(s), nil })
	bin := filepath.Join(t.TempDir(), "muse")
	if err := os.WriteFile(bin, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if museCodeMSPSurfaceOK(bin) || museCodeMSPSurfaceOK(bin) || runs != 1 {
		t.Fatalf("a build without turn/steer qualified (or was not cached): %d runs", runs)
	}
}

// The real export path, against a stand-in binary that writes the bundle the
// way `muse schema generate-json-schema --out DIR` does.
func TestRunMuseMSPSchemaExportReadsTheBundle(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("integration test only runs on win/linux/darwin")
	}
	schema := filepath.Join(t.TempDir(), "msp.schema.json")
	want := rawMuseMSPSchema(loadMuseMSPSurfaceFixture(t))
	if err := os.WriteFile(schema, want, 0o644); err != nil {
		t.Fatal(err)
	}
	bin := mockMSPBinary(t, "muse")
	t.Setenv(mockCLIEnvVar, mockMSPDriverMode)
	t.Setenv(mockMSPSchemaEnv, schema)
	got, err := runMuseMSPSchemaExport(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("export = %q", got)
	}
	t.Setenv(mockMSPSchemaEnv, filepath.Join(t.TempDir(), "absent.json"))
	if _, err := runMuseMSPSchemaExport(bin); err == nil || !strings.Contains(err.Error(), "schema export") {
		t.Fatalf("a failing export: %v", err)
	}
}
