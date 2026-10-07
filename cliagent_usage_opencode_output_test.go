// cliagent_usage_opencode_output_test.go
// -----------------------------------------------------------------------------
// `opencode auth list` prints a DRAWN FRAME, not a plain list. Verbatim from a
// real install:
//
//	"\x1b[0m\n┌  Credentials \x1b[90m~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n"
//
// It also covers the usage rows the parser publishes from the device's own
// ledger (cliagent_usage_opencode_capture.go).
//
// Every one of those lines used to yield a "provider": the bare escape became
// its own row, and the box glyph was the first whitespace-delimited token on the
// others. The card's Account read "[0m, ␍, |, ᴸ" — on a machine with ZERO
// credentials, where the honest answer comes from the model list instead.
// -----------------------------------------------------------------------------

package main

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The exact bytes the CLI produced on the machine that reported the bug.
const realOpenCodeAuthList = "\x1b[0m\n┌  Credentials \x1b[90m~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n"

// The regression.
func TestParseOpenCodeAuthProvidersRejectsFrameDecoration(t *testing.T) {
	got := parseOpenCodeAuthProviders(realOpenCodeAuthList)
	if len(got) != 0 {
		t.Fatalf("providers = %#v, want none — this install has 0 credentials", got)
	}
}

func TestParseOpenCodeAuthProviders(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want []string
	}{
		{"plain rows", "anthropic\nopenai\n", []string{"anthropic", "openai"}},
		{"rows with a method suffix", "anthropic (oauth)\nopenai  api\n", []string{"anthropic", "openai"}},
		{"framed rows", "┌  Credentials ~/x\n│  anthropic (oauth)\n└  1 credentials\n", []string{"anthropic"}},
		{"coloured rows", "\x1b[1manthropic\x1b[0m\n\x1b[90mopenai\x1b[0m\n", []string{"anthropic", "openai"}},
		{"bulleted rows", "• anthropic\n- openai\n* ollama\n", []string{"anthropic", "openai", "ollama"}},
		{"hyphenated provider id", "github-copilot\n", []string{"github-copilot"}},
		{"duplicate rows collapse", "anthropic\nanthropic (oauth)\n", []string{"anthropic"}},
		{"case is normalised", "Anthropic\nANTHROPIC\n", []string{"anthropic"}},
		{"the credentials header is not a provider", "Credentials ~/.local/share/opencode/auth.json\n", nil},
		{"the count footer is not a provider", "0 credentials\n", nil},
		{"a plural-less count footer is not a provider", "1 credential\n", nil},
		{"a no-providers notice is not a provider", "no providers configured\n", nil},
		{"a bare escape is not a provider", "\x1b[0m\n", nil},
		{"box glyphs alone are not providers", "┌\n│\n└\n", nil},
		{"empty output", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOpenCodeAuthProviders(tc.out)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseOpenCodeAuthProviders(%q) = %#v, want %#v", tc.out, got, tc.want)
			}
		})
	}
}

// A token value must never surface as a provider name, decoration or not.
func TestParseOpenCodeAuthProvidersNeverPublishesASecret(t *testing.T) {
	out := "anthropic\nsk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n" +
		"ANTHROPIC_API_KEY=sk-ant-api03-BBBBBBBBBBBBBBBBBBBBBBBBBBBB\n"
	for _, name := range parseOpenCodeAuthProviders(out) {
		if strings.Contains(name, "sk-ant") || strings.Contains(name, "=") {
			t.Fatalf("provider %q looks like a credential", name)
		}
	}
}

func TestStripTerminalDecoration(t *testing.T) {
	tests := []struct{ in, want string }{
		{"\x1b[0m", ""},
		{"\x1b[1manthropic\x1b[0m", "anthropic"},
		{"┌  Credentials", "Credentials"},
		{"│", ""},
		{"└  0 credentials", "0 credentials"},
		{"• opencode/big-pickle", "opencode/big-pickle"},
		{"  ollama/qwen3-coder:30b  ", "ollama/qwen3-coder:30b"},
		{"\x1b[38;5;240mmlx/mlx-community/Qwen3.8-27B-8bit\x1b[m", "mlx/mlx-community/Qwen3.8-27B-8bit"},
		{"plain", "plain"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := stripTerminalDecoration(tc.in); got != tc.want {
			t.Errorf("stripTerminalDecoration(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

/* ─────────────────────────────── model list ─────────────────────────────── */

// Verbatim `opencode models` from the same install.
const realOpenCodeModels = `opencode/big-pickle
opencode/deepseek-v4-flash-free
opencode/hy3-free
mlx/mlx-community/Qwen3.8-27B-8bit
ollama/qwen3-coder:30b
ollama/qwen3.6:35b-a3b
`

func TestParseOpenCodeModelListOnRealOutput(t *testing.T) {
	ids := parseOpenCodeModelList(realOpenCodeModels)
	if len(ids) != 6 {
		t.Fatalf("parsed %d models, want 6: %#v", len(ids), ids)
	}
	// A vendor-namespaced id has two slashes and must survive: rejecting it
	// would silently drop every local mlx model.
	found := false
	for _, id := range ids {
		if id == "mlx/mlx-community/Qwen3.8-27B-8bit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("multi-segment model id was dropped: %#v", ids)
	}
}

func TestParseOpenCodeModelListStripsDecoration(t *testing.T) {
	out := "\x1b[1mopencode/big-pickle\x1b[0m\n• ollama/qwen3-coder:30b\n"
	ids := parseOpenCodeModelList(out)
	want := []string{"opencode/big-pickle", "ollama/qwen3-coder:30b"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %#v, want %#v", ids, want)
	}
}

func TestParseOpenCodeModelListCapsSignedRefreshCatalog(t *testing.T) {
	var output strings.Builder
	for i := 0; i < cliUsageMaxModelsPerProvider+2; i++ {
		fmt.Fprintf(&output, "provider/model-%03d\n", i)
	}

	ids := parseOpenCodeModelList(output.String())
	if len(ids) != cliUsageMaxModelsPerProvider {
		t.Fatalf("parsed %d models, want signed-refresh cap %d", len(ids), cliUsageMaxModelsPerProvider)
	}
	if ids[len(ids)-1] != "provider/model-127" {
		t.Fatalf("last retained model = %q, want stable first %d entries", ids[len(ids)-1], cliUsageMaxModelsPerProvider)
	}
	if _, _, _, err := signCLIUsageRefreshReceipt("secret", "refresh-1", 1, true, []cliAgentUsage{{Provider: "opencode", Models: ids}}, nil); err != nil {
		t.Fatalf("bounded OpenCode catalog rejected from signed refresh: %v", err)
	}
}

// Decoration must not hide a "no providers" notice either — that notice is the
// only thing that lights the red Login-required chip, and a coloured one that
// failed to match would leave a broken install looking fine.
func TestEmptyModelListDetectionSeesThroughDecoration(t *testing.T) {
	if !looksLikeEmptyOpenCodeModelList("\x1b[31mNo providers configured\x1b[0m\n") {
		t.Fatal("a coloured no-providers notice must still be recognised")
	}
}

// A zero-exit `models` that emits only terminal reset codes and whitespace is
// still "named nothing" and must be conclusive. Reset codes like "\x1b[0m"
// survive TrimSpace, so the emptiness check has to run against the
// escape-stripped value or a broken install would appear available.
func TestEmptyModelListDetectionTreatsEscapeOnlyOutputAsEmpty(t *testing.T) {
	if !looksLikeEmptyOpenCodeModelList("\x1b[0m\n") {
		t.Fatal("output that is only reset codes and whitespace must count as empty")
	}
}

/* ──────────────────────── what the card ends up with ─────────────────────── */

// The whole point: with no credentials to name, the card shows the providers and
// models the install can actually reach.
func TestOpenCodeCardFallsBackToModelDerivedProvidersAndListsModels(t *testing.T) {
	ids := parseOpenCodeModelList(realOpenCodeModels)
	providers := parseOpenCodeAuthProviders(realOpenCodeAuthList)
	if len(providers) != 0 {
		t.Fatalf("auth list yielded %#v, want none so the model-derived fallback runs", providers)
	}

	derived := openCodeProvidersFromModelIDs(ids)
	want := []string{"mlx", "ollama", "opencode"}
	if !reflect.DeepEqual(derived, want) {
		t.Fatalf("derived providers = %#v, want %#v", derived, want)
	}
	if len(ids) == 0 {
		t.Fatal("the card must be able to list the reachable models")
	}
}

// A bare path is not a model row. The relaxation that lets
// `mlx/mlx-community/…` through would otherwise admit the auth.json path from
// the Credentials header.
func TestLooksLikeOpenCodeModelIDRejectsPaths(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"opencode/big-pickle", true},
		{"ollama/qwen3-coder:30b", true},
		{"mlx/mlx-community/Qwen3.8-27B-8bit", true},
		{"github-copilot/gpt-5", true},
		{"~/.local/share/opencode/auth.json", false},
		{"/usr/local/bin/opencode", false},
		{"C:/Users/dev/opencode.json", false},
		{"Credentials ~/x", false},
		{"anthropic", false},
		{"anthropic/", false},
		{"/model", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := looksLikeOpenCodeModelID(tc.in); got != tc.want {
			t.Errorf("looksLikeOpenCodeModelID(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The card publishes the reachable models, which is the only content OpenCode
// has to offer — it brokers other providers and meters nothing itself.
func TestOpenCodeParsePublishesTheModelList(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)

	// The test binary stands in for `opencode`, replaying the recorded output
	// (see runMockCLI) — cross-platform, unlike a shell script.
	t.Setenv(mockCLIEnvVar, "opencode")
	usage, ok := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Name: "OpenCode", Version: "1.18.15", Path: os.Args[0],
	}, time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("parser refused a detected install")
	}
	if len(usage.Models) == 0 {
		t.Fatal("usage.Models is empty — the card has nothing to show")
	}
	if usage.Account == "" {
		t.Fatal("usage.Account is empty — expected the model-derived providers")
	}
	for _, m := range usage.Models {
		if !looksLikeOpenCodeModelID(m) {
			t.Errorf("published model id %q is not a model id", m)
		}
	}
	if strings.Contains(usage.Account, "\x1b") || strings.Contains(usage.Account, "┌") {
		t.Fatalf("account %q still carries terminal decoration", usage.Account)
	}
}

/* --------------------------------------------------------------------------
   Published usage rows
   -------------------------------------------------------------------------- */

// The parser used to emit no metric at all, on purpose: OpenCode has no quota
// of its own. It now publishes the device's own "used today" counters, which is
// what the card had nothing numeric to show before.
func TestOpenCodeParse_PublishesTodaysLedgerRows(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	handle := armOpenCodeUsageRun("unit")
	handle.Observe(fmt.Sprintf(`{"type":"step_finish","timestamp":%d,"sessionID":"ses_1",`+
		`"part":{"messageID":"msg_1","type":"step-finish","reason":"stop","cost":0.37,`+
		`"tokens":{"input":100,"output":40,"reasoning":10,"cache":{"read":9000,"write":900}}}}`,
		now.UnixMilli()))
	handle.Finish(true)
	openCodeUsageRefreshWaitFor(2 * time.Second)

	t.Setenv(mockCLIEnvVar, "opencode")
	usage, ok := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Name: "OpenCode", Version: "1.18.15", Path: os.Args[0],
	}, now)
	if !ok {
		t.Fatal("parser refused a detected install")
	}
	if len(usage.Metrics) != 2 {
		t.Fatalf("metrics = %+v, want Tokens today and Cost today", usage.Metrics)
	}
	tokens := usage.Metrics[0]
	// Cache reads and writes are left OUT: counting them would make a cached
	// turn look many times larger than it was.
	if tokens.Consumed == nil || *tokens.Consumed != 150 {
		t.Fatalf("tokens = %v, want 150 (input+output+reasoning, no cache)", tokens.Consumed)
	}
	if tokens.Total != nil || tokens.Unknown {
		t.Fatalf("a limitless counter must carry no total and never read as unknown: %+v", tokens)
	}
	if usage.Metrics[1].Consumed == nil || *usage.Metrics[1].Consumed != 0.37 {
		t.Fatalf("cost = %v, want 0.37", usage.Metrics[1].Consumed)
	}
	if usage.UsageGeneration == nil || usage.UsageGeneration.Counter <= 0 {
		t.Fatalf("usageGeneration = %+v, want the committed capture generation", usage.UsageGeneration)
	}
	// Readiness is unchanged by any of this.
	if len(usage.Models) == 0 || usage.Account == "" || usage.AuthState == "" {
		t.Fatalf("readiness regressed: models=%v account=%q authState=%q",
			usage.Models, usage.Account, usage.AuthState)
	}
	if usage.LoginExpirationState != loginExpirationNotReported {
		t.Fatalf("loginExpirationState = %q, want %q", usage.LoginExpirationState, loginExpirationNotReported)
	}
}

// With nothing it can stand behind, the parser emits NO row — never an
// "unknown" placeholder, which would read as "a limit exists and we cannot see
// it" when there is no OpenCode-level limit at all.
func TestOpenCodeParse_EmitsNoRowWithoutAReading(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	t.Setenv(mockCLIEnvVar, "opencode")
	usage, _ := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Name: "OpenCode", Version: "1.18.15", Path: os.Args[0],
	}, now)
	if len(usage.Metrics) != 0 {
		t.Fatalf("metrics = %+v, want none", usage.Metrics)
	}
	if usage.Notice != "" {
		t.Fatalf("notice = %q, want none", usage.Notice)
	}
}

// A partial day carries the existing card-level notice rather than a new wire
// field, so a low number is never presented as a complete one.
func TestOpenCodeParse_PartialDayCarriesTheLowerBoundNotice(t *testing.T) {
	resetOpenCodeReadinessCache()
	t.Cleanup(resetOpenCodeReadinessCache)
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.Local)
	openCodeUsageFixture(t, now)

	updateOpenCodeUsageLedger(func(l *openCodeUsageLedger) openCodeLedgerEdit {
		openCodeMergeObservations(l, []openCodeObservedMessage{{
			SessionID: "s", MessageID: "m", EventAt: now, Usage: openCodeMessageUsage{In: 3},
		}}, now.UnixMilli())
		openCodeMarkTodayPartial(l, now)
		return openCodeLedgerEdit{Changed: true, TotalsChanged: true}
	})

	t.Setenv(mockCLIEnvVar, "opencode")
	usage, _ := openCodeUsageParser{}.Parse(t.TempDir(), detectedCLIAgent{
		Detected: true, Name: "OpenCode", Version: "1.18.15", Path: os.Args[0],
	}, now)
	if usage.Notice != openCodeUsagePartialNotice || usage.NoticeSeverity != "warning" {
		t.Fatalf("notice = %q/%q, want the lower-bound warning", usage.Notice, usage.NoticeSeverity)
	}
}
