// opencode_variant.go — the reasoning effort, spelled for the OpenCode that is
// installed.
//
// The platform composes an OpenCode effort as `--variant <level>` beside
// `--model provider/model` (shared-constants CLI_AGENT_EFFORT_FLAG_BY_ID).
// That is OpenCode 1's spelling: `opencode run --variant` arrived in 1.1.1.
// OpenCode 2 removed the flag. Its `run` takes the effort INSIDE the model
// reference, `--model provider/model#variant` (anomalyco/opencode
// packages/cli/src/run/run.ts, parseRunModel), and refuses `--variant` while it
// parses options ("Unrecognized flag: --variant in command opencode run"). So
// once a computer upgraded, every run the platform started there with an effort
// failed before it reached a model: chat starts, Ship runs and PR-comment runs
// alike.
//
// OpenCode 2 is also strict about the level, where OpenCode 1 was lenient:
//
//   - A level the model does not offer fails the whole run ("Variant
//     unavailable for <model>: <level>"). OpenCode 1 ignored it. The platform's
//     levels are one fixed span (minimal … xhigh) while each model has its own
//     variants (a local model usually has none), so joining the level blindly
//     would only trade one refusal for another.
//   - A level cannot be given without a model at all ("Cannot select a variant
//     before selecting a model").
//
// So, decided here once for every OpenCode run argv (the direct chat turn and
// the session path):
//
//   - Below OpenCode 2, or when the version is unknown, argv is untouched:
//     `--variant` is what those builds read.
//   - From OpenCode 2 on, every `--variant` is removed and its level is joined
//     to the model reference as `#level`, but only when that model offers the
//     level. The variants come from OpenCode's own model list, as seen from the
//     run's directory (`opencode api model.list`), read at most once per
//     openCodeModelVariantsTTL. If the model does not offer the level, if the
//     list cannot be read, or if no `--model` was named, the level is dropped
//     and the reason is logged. The run then goes ahead at the model's default
//     effort, which is what OpenCode 1 did with a level it could not use.
//
// The version is the one the agent already records (openCodeProbeVersion,
// cached per binary and read again only when the binary changes). A run with no
// effort, or any run on OpenCode 1, therefore asks OpenCode nothing new.
//
// Reading and spelling are separate steps (openCodeEffortReading): the session
// path reads before it takes the session manager's lock, because the model
// list comes from a child process, and spells under the lock from that reading
// alone. The direct chat turn holds no shared lock and does both at once.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// openCodeVariantFlag is OpenCode 1's effort flag. OpenCode 2 has none.
const openCodeVariantFlag = "--variant"

// openCodeInlineVariantMinVersion is the first OpenCode whose `run` reads the
// effort from `--model provider/model#variant` and refuses `--variant`.
const openCodeInlineVariantMinVersion = "2.0.0"

// openCodeReadsInlineVariant reports whether an OpenCode version (a parsed
// major.minor.patch) takes the effort inside the model reference. An unknown
// version ("") keeps the flag: the version probe answers on every build that
// can run at all, so an unknown version is a binary that cannot start either
// way, and leaving argv as it was changes nothing for it.
func openCodeReadsInlineVariant(version string) bool {
	return version != "" && compareSemver(version, openCodeInlineVariantMinVersion) >= 0
}

// openCodeArgsForInstalled spells a run argv's effort for the OpenCode at
// executable (spellOpenCodeEffort), reading the model's variants as OpenCode
// sees them from dir. note is non-empty when a requested level was not applied,
// and says why; callers log it.
func openCodeArgsForInstalled(executable, dir string, args []string) (out []string, note string) {
	return readOpenCodeEffort(executable, dir, args).spell(args)
}

// errOpenCodeEffortNotRead answers a model whose variants a reading never
// asked for: the level is dropped rather than guessed.
var errOpenCodeEffortNotRead = fmt.Errorf("its variants were not read before the run")

// openCodeEffortReading is everything spellOpenCodeEffort asks the installed
// OpenCode for one argv: the recorded version and, for each named model, the
// variants it offers. It is read once (readOpenCodeEffort) and applied later
// with no I/O (spell), so the session path can read before it takes the
// session manager's lock and spell under it. Reading the model list runs a
// child process, and nothing may wait on one under that lock.
type openCodeEffortReading struct {
	version  string
	variants map[string][]string
	errs     map[string]error
}

// readOpenCodeEffort asks the OpenCode at executable exactly what
// spellOpenCodeEffort needs for args, and nothing when args carries no effort.
func readOpenCodeEffort(executable, dir string, args []string) openCodeEffortReading {
	r := openCodeEffortReading{variants: map[string][]string{}, errs: map[string]error{}}
	spellOpenCodeEffort(args,
		func() string {
			r.version = parseOpenCodeVersion(openCodeProbeVersion(executable))
			return r.version
		},
		func(model string) ([]string, error) {
			variants, err := openCodeModelVariants(executable, dir, model)
			r.variants[model], r.errs[model] = variants, err
			return variants, err
		},
	)
	return r
}

// spell applies spellOpenCodeEffort to args from this reading alone. A
// question it did not answer reads as unknown: no version leaves argv as it
// was, and a model whose variants were not read gets no level.
func (r openCodeEffortReading) spell(args []string) (out []string, note string) {
	return spellOpenCodeEffort(args,
		func() string { return r.version },
		func(model string) ([]string, error) {
			if variants, ok := r.variants[model]; ok {
				return variants, r.errs[model]
			}
			return nil, errOpenCodeEffortNotRead
		},
	)
}

// spellOpenCodeEffort is the rule in this file's header, kept free of I/O:
// version is asked only when argv carries a `--variant`, and variantsOf only
// for a model that should receive a level. args is never modified.
//
// Only the synthesized `run --format json …` argv is rewritten. A diagnostic
// invocation (`--version`, `models`, …) passes through verbatim, as everywhere
// else in the OpenCode argv contract.
func spellOpenCodeEffort(args []string, version func() string, variantsOf func(model string) ([]string, error)) (out []string, note string) {
	if !isOpenCodeSynthesizedRun(args) {
		return args, ""
	}
	scan := scanOpenCodeEffort(args)
	if !scan.hasVariant {
		return args, ""
	}
	v := version()
	if !openCodeReadsInlineVariant(v) {
		return args, ""
	}
	out = scan.kept
	level := scan.level
	prefix := fmt.Sprintf("OpenCode %s takes the effort inside --model (provider/model#variant)", v)
	if !openCodeUsableVariantLevel(level) {
		return out, prefix + "; dropped a --variant with no usable level"
	}
	if len(scan.modelAt) == 0 {
		return out, fmt.Sprintf("%s and no model was named, so effort %q was not applied; OpenCode runs its own default model at its default effort", prefix, level)
	}
	var notes []string
	for _, at := range scan.modelAt {
		valueAt, model, ok := openCodeModelValueAt(out, at)
		if !ok {
			notes = append(notes, fmt.Sprintf("a --model with no usable value cannot carry effort %q", level))
			continue
		}
		base := openCodeModelRefBase(model)
		variants, err := variantsOf(base)
		switch {
		case err != nil:
			notes = append(notes, fmt.Sprintf("could not read the variants of %s (%v), so effort %q was not applied", base, err, level))
		case !containsString(variants, level):
			notes = append(notes, fmt.Sprintf("%s offers no %q variant (%s), so it runs at its default effort", base, level, describeOpenCodeVariants(variants)))
		default:
			out[valueAt] = openCodeWithModelValue(out[valueAt], base+"#"+level)
		}
	}
	if len(notes) == 0 {
		return out, ""
	}
	return out, prefix + "; " + strings.Join(notes, "; ")
}

// openCodeEffortScan is what one run argv says about its effort.
type openCodeEffortScan struct {
	// kept is argv with every --variant flag and its value removed.
	kept []string
	// modelAt is the index in kept of each --model / -m flag token.
	modelAt []int
	// level is the LAST --variant's level ("" when it had none), as on the
	// CLI's own command line, where a later occurrence wins.
	level string
	// hasVariant reports whether any --variant was present.
	hasVariant bool
}

// scanOpenCodeEffort walks a run argv the way the builders laid it out: a
// valued flag (openCodeArgvValuedFlag) owns the next token, so a value is never
// read as a flag, and `--` ends the flags.
func scanOpenCodeEffort(args []string) openCodeEffortScan {
	scan := openCodeEffortScan{kept: make([]string, 0, len(args))}
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			scan.kept = append(scan.kept, args[i:]...)
			break
		}
		name, inline := openCodeFlagNameAt(token)
		if name == openCodeVariantFlag {
			scan.hasVariant = true
			scan.level = ""
			if inline {
				scan.level = token[len(name)+1:]
			} else if i+1 < len(args) {
				i++
				scan.level = args[i]
			}
			continue
		}
		scan.kept = append(scan.kept, token)
		if name == "--model" || name == "-m" {
			scan.modelAt = append(scan.modelAt, len(scan.kept)-1)
		}
		if !inline && openCodeArgvValuedFlag(name) && i+1 < len(args) {
			i++
			scan.kept = append(scan.kept, args[i])
		}
	}
	return scan
}

// openCodeArgvValuedFlag reports an OpenCode flag that takes the next argv
// token as its value: the forwarded and the manager-owned tables together, the
// same predicate openCodeNativeLaunchArgs walks a START with.
func openCodeArgvValuedFlag(name string) bool {
	return openCodeForwardedValuedFlags[name] || openCodeValuedStrippedFlags[name]
}

// openCodeUsableVariantLevel reports a level OpenCode 2 can parse after `#`:
// non-empty, no second `#` (Model.Ref.parse refuses one), and not a flag that
// the builder paired with a bare --variant.
func openCodeUsableVariantLevel(level string) bool {
	return level != "" && !strings.Contains(level, "#") && !strings.HasPrefix(level, "-")
}

// openCodeModelValueAt returns where the value of the --model / -m flag at
// kept[at] sits (at itself for the inline `--model=x` form, at+1 for the
// spaced one) and the value. ok is false when there is no value, or the value
// is another flag the builder paired with a bare --model.
func openCodeModelValueAt(kept []string, at int) (valueAt int, model string, ok bool) {
	name, inline := openCodeFlagNameAt(kept[at])
	if inline {
		valueAt, model = at, kept[at][len(name)+1:]
	} else if at+1 < len(kept) {
		valueAt, model = at+1, kept[at+1]
	} else {
		return 0, "", false
	}
	if model == "" || strings.HasPrefix(model, "-") {
		return 0, "", false
	}
	return valueAt, model, true
}

// openCodeWithModelValue replaces the model in a kept token: the value of an
// inline `--model=x` / `-m=x` token, or a spaced value token outright.
func openCodeWithModelValue(token, model string) string {
	if name, inline := openCodeFlagNameAt(token); inline && (name == "--model" || name == "-m") {
		return name + "=" + model
	}
	return model
}

// openCodeModelRefBase is a model reference without its inline variant, split
// where OpenCode 2 splits it (Model.Ref.parse): at the first `#` after the
// provider's `/`. A model id may itself hold `/` and `:`
// (`mlx/mlx-community/Qwen3.8-27B-8bit`, `ollama/qwen3.8:27b`).
func openCodeModelRefBase(ref string) string {
	from := strings.Index(ref, "/") + 1 // 0 when there is no provider
	if hash := strings.Index(ref[from:], "#"); hash >= 0 {
		return ref[:from+hash]
	}
	return ref
}

// describeOpenCodeVariants renders a model's variants for the log line.
func describeOpenCodeVariants(variants []string) string {
	if len(variants) == 0 {
		return "it has no variants"
	}
	return "it offers " + strings.Join(variants, ", ")
}

/* --------------------------------------------------------------------------
   Which variants a model offers: OpenCode's own model list
   -------------------------------------------------------------------------- */

const (
	// openCodeModelVariantsTTL is how long one reading of the model list is
	// reused. Variants change only when a provider or model is configured, so a
	// session's turns share one reading.
	openCodeModelVariantsTTL = 5 * time.Minute
	// openCodeModelVariantsNegativeTTL is how long a FAILED reading is reused,
	// so a broken list costs one attempt per window, not one per turn, and a
	// fixed one is picked up quickly.
	openCodeModelVariantsNegativeTTL = 30 * time.Second
	// openCodeModelVariantsTimeout bounds one `opencode api model.list`. It
	// answers from OpenCode's background service in well under a second; the
	// bound covers starting that service, which the run itself would do next.
	openCodeModelVariantsTimeout = 20 * time.Second
	// openCodeModelVariantsMaxBytes caps the list read. A dozen models are a
	// few KiB; this leaves room for providers that list hundreds.
	openCodeModelVariantsMaxBytes = 16 << 20
)

// openCodeModelVariantsKey identifies one reading: the binary as it is on disk
// (an upgrade reads again) and the directory OpenCode was asked about (a
// project's own configuration can add providers, models and variants).
type openCodeModelVariantsKey struct {
	path    string
	modUnix int64
	size    int64
	dir     string
}

type openCodeModelVariantsEntry struct {
	at       time.Time
	variants map[string][]string
	err      error
}

var (
	openCodeModelVariantsMu    sync.Mutex
	openCodeModelVariantsCache = map[openCodeModelVariantsKey]openCodeModelVariantsEntry{}
	// openCodeModelVariantsNow is the cache's clock (a test seam).
	openCodeModelVariantsNow = time.Now
)

// resetOpenCodeModelVariantsCache forgets every reading (a test seam).
func resetOpenCodeModelVariantsCache() {
	openCodeModelVariantsMu.Lock()
	openCodeModelVariantsCache = map[openCodeModelVariantsKey]openCodeModelVariantsEntry{}
	openCodeModelVariantsMu.Unlock()
}

// openCodeModelVariants returns the variant ids OpenCode offers for model, a
// `provider/model` reference without a variant, as OpenCode sees its models
// from dir. A model OpenCode does not list offers none. err is set only when
// the list itself could not be read.
func openCodeModelVariants(executable, dir, model string) ([]string, error) {
	list, err := openCodeModelVariantList(executable, dir)
	if err != nil {
		return nil, err
	}
	return list[model], nil
}

// openCodeModelVariantList is the cached model list for (binary, dir): every
// listed `provider/model` mapped to its variant ids.
func openCodeModelVariantList(executable, dir string) (map[string][]string, error) {
	key := openCodeModelVariantsKey{path: executable, dir: dir}
	if info, err := os.Stat(executable); err == nil {
		key.modUnix, key.size = info.ModTime().UnixNano(), info.Size()
	}

	openCodeModelVariantsMu.Lock()
	entry, cached := openCodeModelVariantsCache[key]
	openCodeModelVariantsMu.Unlock()
	if cached && openCodeModelVariantsFresh(entry, openCodeModelVariantsNow()) {
		return entry.variants, entry.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), openCodeModelVariantsTimeout)
	defer cancel()
	raw, err := runOpenCodeModelList(ctx, executable, dir)
	var variants map[string][]string
	if err == nil {
		variants, err = parseOpenCodeModelVariants(raw)
	}

	now := openCodeModelVariantsNow()
	openCodeModelVariantsMu.Lock()
	// Expired readings are dropped as new ones land, so a long-lived agent
	// that works in many directories keeps only the recent ones.
	for k, e := range openCodeModelVariantsCache {
		if !openCodeModelVariantsFresh(e, now) {
			delete(openCodeModelVariantsCache, k)
		}
	}
	openCodeModelVariantsCache[key] = openCodeModelVariantsEntry{at: now, variants: variants, err: err}
	openCodeModelVariantsMu.Unlock()
	return variants, err
}

func openCodeModelVariantsFresh(e openCodeModelVariantsEntry, now time.Time) bool {
	ttl := openCodeModelVariantsTTL
	if e.err != nil {
		ttl = openCodeModelVariantsNegativeTTL
	}
	return now.Sub(e.at) < ttl
}

// runOpenCodeModelList runs `opencode api model.list` for dir and returns its
// stdout: OpenCode 2's model list, from its own server, with each model's
// variants. It goes through the one OpenCode launcher (newOpenCodeCmd), so a
// Windows npm shim starts too. A test seam.
//
// The error names the failure only. OpenCode's own output stays out of it,
// because the error is written to the agent log.
var runOpenCodeModelList = func(ctx context.Context, executable, dir string) ([]byte, error) {
	args := []string{"api", "model.list"}
	if dir != "" {
		args = append(args, "--param", "location[directory]="+dir)
	}
	cmd, err := newOpenCodeCmd(ctx, openCodeLaunch{
		Path: executable,
		Args: args,
		Env:  sanitizeOpenCodeEnv(os.Environ()),
		Dir:  dir,
	})
	if err != nil {
		return nil, fmt.Errorf("opencode api model.list could not start: %w", err)
	}
	stdout := &boundedBuffer{limit: openCodeModelVariantsMaxBytes}
	cmd.Stdout = stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("opencode api model.list timed out after %s", openCodeModelVariantsTimeout)
		}
		return nil, fmt.Errorf("opencode api model.list failed: %w", err)
	}
	return stdout.Bytes(), nil
}

// parseOpenCodeModelVariants reads `opencode api model.list`:
//
//	{"location":{…},"data":[{"providerID":"opencode","id":"flint-40b-free",
//	  "variants":[{"id":"low",…},{"id":"high",…}]},…]}
//
// into `provider/model` → variant ids. It reads tolerantly: a line before the
// JSON is skipped, unknown fields are ignored, and variants are accepted both
// as OpenCode 2's array of {id} and as OpenCode 1's object keyed by id.
func parseOpenCodeModelVariants(raw []byte) (map[string][]string, error) {
	start := bytes.IndexByte(raw, '{')
	if start < 0 {
		return nil, fmt.Errorf("opencode api model.list printed no model list")
	}
	var resp struct {
		Data []struct {
			ProviderID string          `json:"providerID"`
			ID         string          `json:"id"`
			Variants   json.RawMessage `json:"variants"`
		} `json:"data"`
	}
	if err := json.NewDecoder(bytes.NewReader(raw[start:])).Decode(&resp); err != nil {
		return nil, fmt.Errorf("opencode api model.list printed an unreadable model list")
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("opencode api model.list printed no model list")
	}
	list := make(map[string][]string, len(resp.Data))
	for _, m := range resp.Data {
		if m.ProviderID == "" || m.ID == "" {
			continue
		}
		ref := m.ProviderID + "/" + m.ID
		for _, id := range openCodeVariantIDs(m.Variants) {
			if !containsString(list[ref], id) {
				list[ref] = append(list[ref], id)
			}
		}
		if _, ok := list[ref]; !ok {
			list[ref] = nil
		}
	}
	return list, nil
}

// openCodeVariantIDs reads one model's variants in either shape: an array of
// objects with an `id` (OpenCode 2), or an object keyed by id (OpenCode 1).
// Anything else reads as no variants.
func openCodeVariantIDs(raw json.RawMessage) []string {
	var ids []string
	var entries []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &entries); err == nil {
		for _, e := range entries {
			if e.ID != "" {
				ids = append(ids, e.ID)
			}
		}
		return ids
	}
	var keyed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keyed); err == nil {
		for id := range keyed {
			if id != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}
	return ids
}
