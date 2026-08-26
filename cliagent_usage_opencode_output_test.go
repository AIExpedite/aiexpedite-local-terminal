// cliagent_usage_opencode_output_test.go
// -----------------------------------------------------------------------------
// `opencode auth list` prints a DRAWN FRAME, not a plain list. Verbatim from a
// real install:
//
//	"\x1b[0m\n┌  Credentials \x1b[90m~/.local/share/opencode/auth.json\n│\n└  0 credentials\n\n"
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
