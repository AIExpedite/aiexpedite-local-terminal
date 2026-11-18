package main

// opencode_variant_test.go — the effort is spelled for the installed OpenCode.
//
// OpenCode 1 reads `--variant <level>`; OpenCode 2 refuses that flag and reads
// the level inside the model reference (`--model provider/model#level`), and
// refuses a level the model does not offer. These pin the rule
// (spellOpenCodeEffort), the model-list reading behind it, and that both spawn
// sites — the direct chat turn and the session path — actually apply it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeReadsInlineVariant(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"", false},
		{"0.9.0", false},
		{"1.1.1", false},
		{"1.19.6", false},
		{"2.0.0", true},
		{"2.2.7", true},
		{"10.0.0", true},
	} {
		if got := openCodeReadsInlineVariant(tc.version); got != tc.want {
			t.Errorf("openCodeReadsInlineVariant(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
	// What `opencode --version` prints, through the recorded-version parser.
	if got := openCodeReadsInlineVariant(parseOpenCodeVersion("opencode v2.2.7")); !got {
		t.Error(`"opencode v2.2.7" must read as OpenCode 2`)
	}
	if got := openCodeReadsInlineVariant(parseOpenCodeVersion("1.19.6")); got {
		t.Error(`"1.19.6" must read as OpenCode 1`)
	}
}

func TestSpellOpenCodeEffort(t *testing.T) {
	// The variants OpenCode 2 reports, keyed as `provider/model` (from a real
	// `opencode api model.list`, trimmed).
	offered := map[string][]string{
		"opencode/flint-40b-free":            {"low", "medium", "high", "xhigh", "max"},
		"opencode/big-pickle":                nil,
		"ollama/qwen3.8:27b-mlx":             nil,
		"mlx/mlx-community/Qwen3.8-27B-8bit": {"low", "medium", "high"},
	}
	unreadable := errors.New("opencode api model.list timed out")

	for _, tc := range []struct {
		name    string
		version string
		listErr error
		in      []string
		want    []string
		// noteHas is a fragment the log note must carry; "" means no note.
		noteHas string
		// asks is the models whose variants must be asked for, in order.
		asks []string
		// versionAsked is whether the recorded version had to be read.
		versionAsked bool
	}{
		{
			name:    "no effort: nothing is read and argv is untouched",
			version: "2.2.7",
			in:      []string{"run", "--format", "json", "--model", "opencode/big-pickle"},
			want:    []string{"run", "--format", "json", "--model", "opencode/big-pickle"},
		},
		{
			name:         "OpenCode 1 keeps --variant",
			version:      "1.19.6",
			in:           []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant", "high"},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant", "high"},
			versionAsked: true,
		},
		{
			name:         "an unknown version keeps --variant",
			version:      "",
			in:           []string{"run", "--format", "json", "--variant", "xhigh"},
			want:         []string{"run", "--format", "json", "--variant", "xhigh"},
			versionAsked: true,
		},
		{
			name:         "OpenCode 2 joins an offered level to the model",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant", "high"},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free#high"},
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			name:         "OpenCode 2 keeps a resumed session and joins the level",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--session", "ses_1", "--model", "opencode/flint-40b-free", "--variant", "low"},
			want:         []string{"run", "--format", "json", "--session", "ses_1", "--model", "opencode/flint-40b-free#low"},
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			name:         "a level the model does not offer is dropped, not sent",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "ollama/qwen3.8:27b-mlx", "--variant", "high"},
			want:         []string{"run", "--format", "json", "--model", "ollama/qwen3.8:27b-mlx"},
			noteHas:      `ollama/qwen3.8:27b-mlx offers no "high" variant (it has no variants)`,
			asks:         []string{"ollama/qwen3.8:27b-mlx"},
			versionAsked: true,
		},
		{
			name:         "a level a model with other variants lacks is dropped",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "mlx/mlx-community/Qwen3.8-27B-8bit", "--variant", "xhigh"},
			want:         []string{"run", "--format", "json", "--model", "mlx/mlx-community/Qwen3.8-27B-8bit"},
			noteHas:      "it offers low, medium, high",
			asks:         []string{"mlx/mlx-community/Qwen3.8-27B-8bit"},
			versionAsked: true,
		},
		{
			name:         "an unreadable model list drops the level",
			version:      "2.2.7",
			listErr:      unreadable,
			in:           []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant", "high"},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free"},
			noteHas:      "could not read the variants of opencode/flint-40b-free",
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			// The chat start in the 2026-11-12 failure: `launch=[--variant medium]`.
			name:         "no model to join it to: the level is dropped",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--variant", "medium"},
			want:         []string{"run", "--format", "json"},
			noteHas:      `no model was named, so effort "medium" was not applied`,
			versionAsked: true,
		},
		{
			name:         "the inline spellings of both flags",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model=opencode/flint-40b-free", "--variant=max"},
			want:         []string{"run", "--format", "json", "--model=opencode/flint-40b-free#max"},
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			name:         "the short -m spellings",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "-m", "opencode/flint-40b-free", "--variant", "medium"},
			want:         []string{"run", "--format", "json", "-m", "opencode/flint-40b-free#medium"},
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			name:         "-m=value",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "-m=mlx/mlx-community/Qwen3.8-27B-8bit", "--variant", "low"},
			want:         []string{"run", "--format", "json", "-m=mlx/mlx-community/Qwen3.8-27B-8bit#low"},
			asks:         []string{"mlx/mlx-community/Qwen3.8-27B-8bit"},
			versionAsked: true,
		},
		{
			name:         "the requested level replaces an inline one",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "opencode/flint-40b-free#low", "--variant", "high"},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free#high"},
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			name:         "an inline level stays when the requested one is not offered",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "mlx/mlx-community/Qwen3.8-27B-8bit#low", "--variant", "max"},
			want:         []string{"run", "--format", "json", "--model", "mlx/mlx-community/Qwen3.8-27B-8bit#low"},
			noteHas:      `offers no "max" variant`,
			asks:         []string{"mlx/mlx-community/Qwen3.8-27B-8bit"},
			versionAsked: true,
		},
		{
			name:         "the last --variant wins",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--variant", "low", "--model", "opencode/flint-40b-free", "--variant", "xhigh"},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free#xhigh"},
			asks:         []string{"opencode/flint-40b-free"},
			versionAsked: true,
		},
		{
			name:         "a bare trailing --variant is dropped",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant"},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free"},
			noteHas:      "dropped a --variant with no usable level",
			versionAsked: true,
		},
		{
			name:         "an empty inline --variant= is dropped",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant="},
			want:         []string{"run", "--format", "json", "--model", "opencode/flint-40b-free"},
			noteHas:      "dropped a --variant with no usable level",
			versionAsked: true,
		},
		{
			// buildOpenCodeInteractiveArgs pairs a learned valued flag with the
			// next token whatever it looks like; that token is a value here.
			name:    "a valued flag's value is never read as --variant",
			version: "2.2.7",
			in:      []string{"run", "--format", "json", "--agent", "--variant", "--model", "opencode/big-pickle"},
			want:    []string{"run", "--format", "json", "--agent", "--variant", "--model", "opencode/big-pickle"},
		},
		{
			name:         "a --model with no usable value cannot carry the level",
			version:      "2.2.7",
			in:           []string{"run", "--format", "json", "--variant", "high", "--model"},
			want:         []string{"run", "--format", "json", "--model"},
			noteHas:      "a --model with no usable value",
			versionAsked: true,
		},
		{
			name:    "a diagnostic invocation passes through verbatim",
			version: "2.2.7",
			in:      []string{"--version", "--variant", "high"},
			want:    []string{"--version", "--variant", "high"},
		},
		{
			name:    "nothing after -- is a flag",
			version: "2.2.7",
			in:      []string{"run", "--format", "json", "--", "--variant", "high"},
			want:    []string{"run", "--format", "json", "--", "--variant", "high"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := append([]string(nil), tc.in...)
			versionAsked := false
			var asks []string
			got, note := spellOpenCodeEffort(in,
				func() string { versionAsked = true; return tc.version },
				func(model string) ([]string, error) {
					asks = append(asks, model)
					if tc.listErr != nil {
						return nil, tc.listErr
					}
					return offered[model], nil
				})
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
			if strings.Join(in, " ") != strings.Join(tc.in, " ") {
				t.Errorf("the caller's argv was modified: %q", in)
			}
			switch {
			case tc.noteHas == "" && note != "":
				t.Errorf("unexpected note %q", note)
			case tc.noteHas != "" && !strings.Contains(note, tc.noteHas):
				t.Errorf("note %q does not say %q", note, tc.noteHas)
			}
			if strings.Join(asks, ",") != strings.Join(tc.asks, ",") {
				t.Errorf("asked for the variants of %q, want %q", asks, tc.asks)
			}
			if versionAsked != tc.versionAsked {
				t.Errorf("version read = %v, want %v", versionAsked, tc.versionAsked)
			}
		})
	}
}

func TestOpenCodeModelRefBase(t *testing.T) {
	for in, want := range map[string]string{
		"opencode/big-pickle":                        "opencode/big-pickle",
		"opencode/flint-40b-free#high":               "opencode/flint-40b-free",
		"ollama/qwen3.8:27b-mlx":                     "ollama/qwen3.8:27b-mlx",
		"mlx/mlx-community/Qwen3.8-27B-8bit#low":     "mlx/mlx-community/Qwen3.8-27B-8bit",
		"anthropic/claude-sonnet-4-5@20250929#think": "anthropic/claude-sonnet-4-5@20250929",
		"no-provider#high":                           "no-provider",
	} {
		if got := openCodeModelRefBase(in); got != want {
			t.Errorf("openCodeModelRefBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOpenCodeModelVariants(t *testing.T) {
	t.Run("OpenCode 2's model list", func(t *testing.T) {
		raw := `{"location":{"directory":"/w"},"data":[` +
			`{"id":"flint-40b-free","providerID":"opencode","variants":[{"id":"low","settings":{}},{"id":"high"}]},` +
			`{"id":"qwen3.8:27b-mlx","providerID":"ollama","variants":[]},` +
			`{"id":"mlx-community/Qwen3.8-27B-8bit","providerID":"mlx","variants":[{"id":"low"},{"id":"low"},{"id":""}]},` +
			`{"id":"","providerID":"broken"}]}` + "\n"
		got, err := parseOpenCodeModelVariants([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if v := strings.Join(got["opencode/flint-40b-free"], ","); v != "low,high" {
			t.Errorf("flint variants = %q", v)
		}
		if v, listed := got["ollama/qwen3.8:27b-mlx"]; !listed || len(v) != 0 {
			t.Errorf("a model with no variants must be listed with none, got %q (listed %v)", v, listed)
		}
		if v := strings.Join(got["mlx/mlx-community/Qwen3.8-27B-8bit"], ","); v != "low" {
			t.Errorf("duplicate and empty ids must collapse, got %q", v)
		}
		if len(got) != 3 {
			t.Errorf("a model without an id must be skipped, got %v", got)
		}
	})
	t.Run("variants keyed by id, after a line of noise", func(t *testing.T) {
		raw := "warning: plugin slow to load\n" +
			`{"data":[{"providerID":"p","id":"m","variants":{"max":{},"high":{}}}]}`
		got, err := parseOpenCodeModelVariants([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if v := strings.Join(got["p/m"], ","); v != "high,max" {
			t.Errorf("keyed variants = %q", v)
		}
	})
	for name, raw := range map[string]string{
		"empty output":      "",
		"not JSON":          "HTTP 404 Not Found",
		"a truncated list":  `{"data":[{"providerID":"p","id":"m","varia`,
		"no data in answer": `{"location":{"directory":"/w"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseOpenCodeModelVariants([]byte(raw)); err == nil {
				t.Fatal("an unusable answer must be an error, so the level is dropped")
			}
		})
	}
}

// stubOpenCodeModelList replaces the `opencode api model.list` runner and
// returns the directories it was asked about.
func stubOpenCodeModelList(t *testing.T, answer func(dir string) ([]byte, error)) *[]string {
	t.Helper()
	original := runOpenCodeModelList
	var dirs []string
	runOpenCodeModelList = func(_ context.Context, _ string, dir string) ([]byte, error) {
		dirs = append(dirs, dir)
		return answer(dir)
	}
	resetOpenCodeModelVariantsCache()
	t.Cleanup(func() {
		runOpenCodeModelList = original
		resetOpenCodeModelVariantsCache()
	})
	return &dirs
}

func TestOpenCodeModelVariants_OneReadingPerWindow(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(binary, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	original := openCodeModelVariantsNow
	openCodeModelVariantsNow = func() time.Time { return now }
	t.Cleanup(func() { openCodeModelVariantsNow = original })

	fail := false
	dirs := stubOpenCodeModelList(t, func(string) ([]byte, error) {
		if fail {
			return nil, errors.New("opencode api model.list failed: exit status 1")
		}
		return []byte(`{"data":[{"providerID":"p","id":"m","variants":[{"id":"high"}]}]}`), nil
	})
	ask := func(dir string) ([]string, error) { return openCodeModelVariants(binary, dir, "p/m") }

	if v, err := ask("/w1"); err != nil || strings.Join(v, ",") != "high" {
		t.Fatalf("first reading = %q, %v", v, err)
	}
	if _, err := ask("/w1"); err != nil || len(*dirs) != 1 {
		t.Fatalf("a second turn within the window must reuse the reading, read %d times (%v)", len(*dirs), err)
	}
	if v, err := openCodeModelVariants(binary, "/w1", "p/unlisted"); err != nil || len(v) != 0 || len(*dirs) != 1 {
		t.Fatalf("an unlisted model offers nothing, is no error and reads nothing new: %q, %v", v, err)
	}

	// Another directory is another reading: a project can add its own models.
	if _, err := ask("/w2"); err != nil || len(*dirs) != 2 || (*dirs)[1] != "/w2" {
		t.Fatalf("another directory must be read on its own: %q (%v)", *dirs, err)
	}

	// The window ends: read again.
	now = now.Add(openCodeModelVariantsTTL)
	if _, err := ask("/w1"); err != nil || len(*dirs) != 3 {
		t.Fatalf("an expired reading must be read again, read %d times (%v)", len(*dirs), err)
	}

	// A replaced binary (an upgrade) is read again inside the window.
	if err := os.WriteFile(binary, []byte("v2 is longer"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ask("/w1"); err != nil || len(*dirs) != 4 {
		t.Fatalf("an upgraded binary must be read again, read %d times (%v)", len(*dirs), err)
	}

	// A failure is remembered only for the short window.
	fail = true
	now = now.Add(openCodeModelVariantsTTL)
	if _, err := ask("/w1"); err == nil || len(*dirs) != 5 {
		t.Fatalf("a failed reading must be an error, read %d times (%v)", len(*dirs), err)
	}
	if _, err := ask("/w1"); err == nil || len(*dirs) != 5 {
		t.Fatalf("a failure must be reused within its window, read %d times", len(*dirs))
	}
	fail = false
	now = now.Add(openCodeModelVariantsNegativeTTL)
	if v, err := ask("/w1"); err != nil || len(v) != 1 || len(*dirs) != 6 {
		t.Fatalf("a fixed list must be picked up after the failure window: %q, %v, read %d times", v, err, len(*dirs))
	}
}

/* --------------------------------------------------------------------------
   Both spawn sites, against the compiled stub
   -------------------------------------------------------------------------- */

// openCodeStubModelList is a `model.list` answer in OpenCode 2's shape.
const openCodeStubModelList = `{"location":{"directory":"/w"},"data":[` +
	`{"id":"flint-40b-free","providerID":"opencode","variants":[{"id":"low"},{"id":"medium"},{"id":"high"}]},` +
	`{"id":"qwen3.8:27b-mlx","providerID":"ollama","variants":[]}]}`

// installOpenCodeStubAt installs the stub reporting version, answering
// `api model.list` with openCodeStubModelList, and returns the argv log of its
// runs and the log of its api calls.
func installOpenCodeStubAt(t *testing.T, version string) (argvLog, apiLog string) {
	t.Helper()
	installOpenCodeStub(t)
	dir := t.TempDir()
	argvLog = filepath.Join(dir, "argv.log")
	apiLog = filepath.Join(dir, "api.log")
	t.Setenv("OPENCODE_STUB_VERSION", version)
	t.Setenv("OPENCODE_STUB_ARGV_LOG", argvLog)
	t.Setenv("OPENCODE_STUB_API_LOG", apiLog)
	t.Setenv("OPENCODE_STUB_API", openCodeStubModelList)
	t.Setenv("OPENCODE_STUB_STDOUT", `{"type":"text","text":"ok"}`+"\\n"+`{"type":"session.completed"}`+"\\n")
	return argvLog, apiLog
}

func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestOpenCodeNativeTurn_SpellsTheEffortForTheInstalledVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		version  string
		launch   []string
		wantArgv string
		// wantAPI is whether the model list must have been read.
		wantAPI bool
	}{
		{
			name:     "OpenCode 2: an offered level joins the model",
			version:  "opencode v2.2.7",
			launch:   []string{"--model", "opencode/flint-40b-free", "--variant", "high"},
			wantArgv: "run --format json --model opencode/flint-40b-free#high",
			wantAPI:  true,
		},
		{
			name:     "OpenCode 2: a level the model lacks is dropped",
			version:  "opencode v2.2.7",
			launch:   []string{"--model", "ollama/qwen3.8:27b-mlx", "--variant", "high"},
			wantArgv: "run --format json --model ollama/qwen3.8:27b-mlx",
			wantAPI:  true,
		},
		{
			name:     "OpenCode 2: no model, so the level is dropped",
			version:  "opencode v2.2.7",
			launch:   []string{"--variant", "medium"},
			wantArgv: "run --format json",
		},
		{
			name:     "OpenCode 1: --variant as before",
			version:  "1.19.6",
			launch:   []string{"--model", "opencode/flint-40b-free", "--variant", "high"},
			wantArgv: "run --format json --model opencode/flint-40b-free --variant high",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvLog, apiLog := installOpenCodeStubAt(t, tc.version)
			m := NewOpenCodeNativeManager()
			id := "sess-effort"
			cwd := t.TempDir()
			sess := injectOpenCodeSession(t, m, id, cwd)
			sess.LaunchArgs = openCodeNativeLaunchArgs(tc.launch)

			if err := m.Send(id, "say ok", func(resultMsg) {}, 60*time.Second); err != nil {
				t.Fatalf("Send failed: %v", err)
			}
			runs := readLogLines(t, argvLog)
			if len(runs) != 1 || runs[0] != tc.wantArgv {
				t.Fatalf("the turn ran %q, want [%q]", runs, tc.wantArgv)
			}
			api := readLogLines(t, apiLog)
			if !tc.wantAPI {
				if len(api) != 0 {
					t.Fatalf("the model list must not be read here, read %q", api)
				}
				return
			}
			runDir, err := containedCwd(cwd, "")
			if err != nil {
				t.Fatal(err)
			}
			want := "api model.list --param location[directory]=" + runDir
			if len(api) != 1 || api[0] != want {
				t.Fatalf("model list read as %q, want [%q]", api, want)
			}
		})
	}
}

func TestOpenCodeNativeTurn_ReadsTheModelListOncePerSession(t *testing.T) {
	argvLog, apiLog := installOpenCodeStubAt(t, "opencode v2.2.7")
	m := NewOpenCodeNativeManager()
	id := "sess-effort-turns"
	sess := injectOpenCodeSession(t, m, id, t.TempDir())
	sess.LaunchArgs = openCodeNativeLaunchArgs([]string{"--model", "opencode/flint-40b-free", "--variant", "low"})

	for turn := 1; turn <= 3; turn++ {
		if err := m.Send(id, fmt.Sprintf("turn %d", turn), func(resultMsg) {}, 60*time.Second); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
	}
	runs := readLogLines(t, argvLog)
	if len(runs) != 3 {
		t.Fatalf("want 3 runs, got %q", runs)
	}
	for _, run := range runs {
		if !strings.Contains(run, "--model opencode/flint-40b-free#low") || strings.Contains(run, "--variant") {
			t.Fatalf("every turn carries the joined level and no --variant, got %q", run)
		}
	}
	if api := readLogLines(t, apiLog); len(api) != 1 {
		t.Fatalf("the model list must be read once for the session's turns, read %d times", len(api))
	}
}

func TestOpenCodeSessionStart_SpellsTheEffortForTheInstalledVersion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		version  string
		args     []string
		wantArgv string
	}{
		{
			// The Ship run in the 2026-11-12 failure:
			// `opencode run --format json --variant xhigh`.
			name:     "OpenCode 2: an effort with no model runs without it",
			version:  "opencode v2.2.7",
			args:     []string{"--variant", "xhigh", "address the review comments"},
			wantArgv: "run --format json",
		},
		{
			name:     "OpenCode 2: an offered level joins the model",
			version:  "opencode v2.2.7",
			args:     []string{"--model", "opencode/flint-40b-free", "--variant", "medium", "address the review comments"},
			wantArgv: "run --format json --model opencode/flint-40b-free#medium",
		},
		{
			name:     "OpenCode 1: --variant as before",
			version:  "1.19.6",
			args:     []string{"--model", "opencode/flint-40b-free", "--variant", "medium", "address the review comments"},
			wantArgv: "run --format json --model opencode/flint-40b-free --variant medium",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvLog, _ := installOpenCodeStubAt(t, tc.version)
			sm := NewSessionManager(nil)
			id := fmt.Sprintf("opencode-effort-%d", time.Now().UnixNano())
			ended, publishFn := openCodeSessionEndedSignal()
			if err := sm.StartSession(id, "opencode", tc.args, t.TempDir(), "ws", "uid", 30000, false, publishFn); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			select {
			case <-ended:
			case <-time.After(30 * time.Second):
				t.Fatal("the session never ended")
			}
			runs := readLogLines(t, argvLog)
			if len(runs) != 1 || runs[0] != tc.wantArgv {
				t.Fatalf("the session ran %q, want [%q]", runs, tc.wantArgv)
			}
		})
	}
}

func TestOpenCodeEffortReading_SpellsOnlyWhatItRead(t *testing.T) {
	in := []string{"run", "--format", "json", "--model", "opencode/flint-40b-free", "--variant", "high"}

	read := openCodeEffortReading{
		version:  "2.2.7",
		variants: map[string][]string{"opencode/flint-40b-free": {"low", "high"}},
		errs:     map[string]error{},
	}
	if got, note := read.spell(in); strings.Join(got, " ") != "run --format json --model opencode/flint-40b-free#high" || note != "" {
		t.Fatalf("a read model must be spelled from the reading: %q (%q)", got, note)
	}

	other := []string{"run", "--format", "json", "--model", "opencode/big-pickle", "--variant", "high"}
	if got, note := read.spell(other); strings.Join(got, " ") != "run --format json --model opencode/big-pickle" ||
		!strings.Contains(note, "were not read before the run") {
		t.Fatalf("a model the reading never asked about must get no level: %q (%q)", got, note)
	}

	var none openCodeEffortReading
	if got, note := none.spell(in); strings.Join(got, " ") != strings.Join(in, " ") || note != "" {
		t.Fatalf("an empty reading (no version) must leave argv as it was: %q (%q)", got, note)
	}
}

// The model list comes from a child process, and the session manager's lock
// must never wait on one: every other session operation, the tray's shutdown
// snapshot included, queues behind it.
func TestOpenCodeSessionStart_ReadsTheModelListOutsideTheManagerLock(t *testing.T) {
	argvLog, _ := installOpenCodeStubAt(t, "opencode v2.2.7")
	sm := NewSessionManager(nil)
	lockHeld := 0
	reads := stubOpenCodeModelList(t, func(string) ([]byte, error) {
		if sm.mu.TryLock() {
			sm.mu.Unlock()
		} else {
			lockHeld++
		}
		return []byte(openCodeStubModelList), nil
	})

	id := fmt.Sprintf("opencode-effort-lock-%d", time.Now().UnixNano())
	ended, publishFn := openCodeSessionEndedSignal()
	args := []string{"--model", "opencode/flint-40b-free", "--variant", "medium", "address the review comments"}
	if err := sm.StartSession(id, "opencode", args, t.TempDir(), "ws", "uid", 30000, false, publishFn); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the session never ended")
	}
	if len(*reads) != 1 {
		t.Fatalf("the model list must be read once, read %d times", len(*reads))
	}
	if lockHeld != 0 {
		t.Fatal("the model list was read while the session manager's lock was held")
	}
	if runs := readLogLines(t, argvLog); len(runs) != 1 || runs[0] != "run --format json --model opencode/flint-40b-free#medium" {
		t.Fatalf("the session ran %q", runs)
	}
}
