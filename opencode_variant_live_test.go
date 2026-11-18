package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

/* --------------------------------------------------------------------------
   opencode_variant_live_test.go — the live acceptance for the effort spelling
   (opencode_variant.go) against the REAL OpenCode on this computer.
   --------------------------------------------------------------------------
   OPT-IN, never part of CI. It spends real `opencode run` turns against the
   computer's own OpenCode login and default model, so it runs only when

       AIEXPEDITE_LIVE_OPENCODE=1

   is set:

       go test -run TestOpenCodeEffortLive -count=1 -v -timeout 30m .

   It drives both spawn sites the way the platform does: the chat start with
   an effort and no model (`launch=[--variant medium]`), the session path with
   an effort and no model (`opencode run --format json --variant xhigh`, a Ship
   run), and, when AIEXPEDITE_LIVE_OPENCODE_MODEL=<provider/model> names a
   model, a level that model offers and one it does not. Each case must finish
   a real turn: on OpenCode 2 every one of them failed before this change.

   It logs the argv flags, levels and model ids only, never a prompt or a reply.
   ------------------------------------------------------------------------ */

// openCodeLiveLevels is every effort level the platform can send, in the order
// the live test tries them when it needs one a model does not offer.
var openCodeLiveLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

const openCodeLivePrompt = "Reply with exactly the word OK and nothing else. Do not use any tools."

func TestOpenCodeEffortLive(t *testing.T) {
	if os.Getenv("AIEXPEDITE_LIVE_OPENCODE") != "1" {
		t.Skip("set AIEXPEDITE_LIVE_OPENCODE=1 to run the live OpenCode effort test")
	}
	// OpenCode keeps its login, provider config and background service in the
	// real profile.
	useRealUserProfile(t)
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode is not on PATH")
	}
	resetVersionProbeCache()
	resetOpenCodeCapabilityCache()
	resetOpenCodeModelVariantsCache()
	t.Cleanup(func() {
		resetVersionProbeCache()
		resetOpenCodeCapabilityCache()
		resetOpenCodeModelVariantsCache()
	})

	executable := resolveOpenCodeExecutable()
	version := parseOpenCodeVersion(openCodeProbeVersion(executable))
	t.Logf("OpenCode %s at %s (reads the effort inside --model: %v)", version, executable, openCodeReadsInlineVariant(version))
	if version == "" {
		t.Fatal("the installed OpenCode reported no version")
	}

	t.Run("chat start: an effort and no model", func(t *testing.T) {
		argv := runOpenCodeLiveNativeTurn(t, []string{"--variant", "medium"})
		if openCodeReadsInlineVariant(version) && strings.Contains(argv, "--variant") {
			t.Fatalf("OpenCode %s was handed --variant: %s", version, argv)
		}
	})

	t.Run("session path: an effort and no model", func(t *testing.T) {
		runOpenCodeLiveSession(t, []string{"--variant", "xhigh", openCodeLivePrompt})
	})

	model := os.Getenv("AIEXPEDITE_LIVE_OPENCODE_MODEL")
	if model == "" {
		t.Log("AIEXPEDITE_LIVE_OPENCODE_MODEL is unset: skipping the model-named cases")
		return
	}
	dir := t.TempDir()
	variants, err := openCodeModelVariants(executable, dir, model)
	if err != nil && openCodeReadsInlineVariant(version) {
		t.Fatalf("could not read %s's variants: %v", model, err)
	}
	t.Logf("%s offers %q", model, variants)
	offeredLevel, missingLevel := "", ""
	for _, level := range openCodeLiveLevels {
		if containsString(variants, level) {
			if offeredLevel == "" {
				offeredLevel = level
			}
		} else if missingLevel == "" {
			missingLevel = level
		}
	}

	t.Run("an offered level joins the model", func(t *testing.T) {
		if offeredLevel == "" {
			t.Skipf("%s offers none of %q", model, openCodeLiveLevels)
		}
		argv := runOpenCodeLiveNativeTurn(t, []string{"--model", model, "--variant", offeredLevel})
		if openCodeReadsInlineVariant(version) && !strings.Contains(argv, "--model "+model+"#"+offeredLevel) {
			t.Fatalf("the level was not joined to the model: %s", argv)
		}
	})

	t.Run("a level the model does not offer is dropped", func(t *testing.T) {
		if missingLevel == "" {
			t.Skipf("%s offers every level the platform sends", model)
		}
		argv := runOpenCodeLiveNativeTurn(t, []string{"--model", model, "--variant", missingLevel})
		if openCodeReadsInlineVariant(version) && (strings.Contains(argv, "#") || strings.Contains(argv, "--variant")) {
			t.Fatalf("an unoffered level reached OpenCode %s: %s", version, argv)
		}
	})

	t.Run("session path: an offered level joins the model", func(t *testing.T) {
		if offeredLevel == "" {
			t.Skipf("%s offers none of %q", model, openCodeLiveLevels)
		}
		runOpenCodeLiveSession(t, []string{"--model", model, "--variant", offeredLevel, openCodeLivePrompt})
	})
}

// runOpenCodeLiveNativeTurn starts a direct chat session the way
// opencode_native_start does (the START's args become every turn's launch
// flags), sends one real turn, and requires it to complete with a reply. It
// returns the argv the turn was spelled with.
func runOpenCodeLiveNativeTurn(t *testing.T, startArgs []string) string {
	t.Helper()
	dir := openCodeLiveProject(t)
	m := NewOpenCodeNativeManager()
	id := fmt.Sprintf("live-effort-%d", time.Now().UnixNano())
	sess := injectOpenCodeSession(t, m, id, dir)
	sess.LaunchArgs = openCodeNativeLaunchArgs(startArgs)

	runDir, err := containedCwd(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	spelled, note := openCodeArgsForInstalled(resolveOpenCodeExecutable(), runDir,
		buildOpenCodeNativeTurnArgs("", sess.LaunchArgs))
	argv := strings.Join(spelled, " ")
	t.Logf("start %q -> turn argv %q", startArgs, argv)
	if note != "" {
		t.Logf("note: %s", note)
	}

	var mu sync.Mutex
	var errorsSeen []string
	completed := false
	err = m.Send(id, openCodeLivePrompt, func(r resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		if r.Status == "error" || strings.HasSuffix(r.Type, "_error") {
			errorsSeen = append(errorsSeen, r.Type)
		}
		if r.Type == "opencode_native_message" && strings.Contains(r.Output, `"text"`) {
			completed = true
		}
	}, 10*time.Minute)
	mu.Lock()
	defer mu.Unlock()
	if err != nil || len(errorsSeen) > 0 {
		t.Fatalf("the turn failed (frames %q): %v", errorsSeen, err)
	}
	if !completed {
		t.Fatal("the turn finished without a reply")
	}
	return argv
}

// runOpenCodeLiveSession starts an orchestrated session the way session_start
// does and requires the real CLI to exit 0 having streamed output.
func runOpenCodeLiveSession(t *testing.T, args []string) {
	t.Helper()
	dir := openCodeLiveProject(t)
	sm := NewSessionManager(nil)
	id := fmt.Sprintf("live-effort-session-%d", time.Now().UnixNano())

	var mu sync.Mutex
	exitCode, streamed := -1, 0
	ended := make(chan struct{})
	var once sync.Once
	publishFn := func(r resultMsg) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Type {
		case "session_ended":
			exitCode = r.ExitCode
			once.Do(func() { close(ended) })
		case "stream":
			streamed++
		}
	}
	if err := sm.StartSession(id, "opencode", args, dir, "ws", "uid", int64((10 * time.Minute).Milliseconds()), false, publishFn); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(11 * time.Minute):
		_ = sm.EndSession(id)
		t.Fatal("the session never ended")
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("session %q exited %d after %d stream frames", args[:len(args)-1], exitCode, streamed)
	if exitCode != 0 {
		t.Fatalf("opencode exited %d", exitCode)
	}
	if streamed == 0 {
		t.Fatal("opencode exited without output")
	}
}

// openCodeLiveProject is a fresh git repository: OpenCode treats a directory
// with no project root as external and refuses file access there.
func openCodeLiveProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}
