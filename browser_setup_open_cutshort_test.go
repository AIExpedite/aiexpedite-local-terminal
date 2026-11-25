package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A Chrome check the op's own deadline cut short proves nothing about this
// computer, so it must never be reported as "Chrome isn't installed".
//
// The real shape on macOS: verifyChromeExecutable gives `codesign --verify` a
// 20 s ceiling, but the whole op is BROWSER_SETUP_OPEN_LIMITS.DEADLINE_MS
// (15 s) less browserResultMargin (1.5 s) less delivery — so the parent
// context, not that ceiling, ends a slow check, and the candidate used to be
// skipped exactly like one whose signature was wrong. A macOS user with
// Chrome installed was then shown a download link and no way to retry.
func TestChromeCheckCutShortIsNotReportedAsNotInstalled(t *testing.T) {
	mac := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	exists := func(string) bool { return true }

	// verify behaves as exec.CommandContext does when the parent context ends:
	// it returns an error, and ctx.Err() is set.
	cancelledVerify := func(ctx context.Context, _ string, _ string) error {
		return ctx.Err()
	}

	t.Run("resolve reports the context error rather than an empty verdict", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := resolveChromeExecutable(ctx, "darwin", []string{mac}, exists, cancelledVerify)
		if got != "" {
			t.Fatalf("expected no executable, got %q", got)
		}
		if err == nil {
			t.Fatal("a check the deadline cut short must not answer a nil error: " +
				"that is the one state meaning Chrome is genuinely absent")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected the context error, got %v", err)
		}
	})

	t.Run("a later candidate cannot be skipped by a cut-short earlier one", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// Evil.app is checked first and its check is cut short; the real
		// Chrome behind it was never evaluated, so the answer is undetermined.
		got, err := resolveChromeExecutable(ctx, "darwin",
			[]string{"/Applications/Evil.app/Contents/MacOS/chrome", mac}, exists, cancelledVerify)
		if got != "" || err == nil {
			t.Fatalf("expected an undetermined answer, got %q / %v", got, err)
		}
	})

	t.Run("open answers a retryable error, never chrome_not_installed", func(t *testing.T) {
		o := &browserSetupOpener{
			goos: "darwin",
			findChrome: func(context.Context) (string, error) {
				return "", context.DeadlineExceeded
			},
			start: func(context.Context, []string) error {
				t.Fatal("nothing may be launched when Chrome was never resolved")
				return nil
			},
		}
		res, berr := o.open(context.Background(), browserSetupChromeRemoteDebugging)
		if berr == nil {
			t.Fatalf("expected a retryable error, got outcome %v", res)
		}
		if res != nil {
			t.Fatalf("an error answer must carry no outcome, got %v", res)
		}
		if berr.Code != browserErrChromeNotReachable {
			t.Fatalf("expected %q, got %q", browserErrChromeNotReachable, berr.Code)
		}
		if strings.Contains(strings.ToLower(berr.Message), "not installed") {
			t.Fatalf("the message must not claim Chrome is absent: %q", berr.Message)
		}
	})

	t.Run("a genuinely absent Chrome is still reported as not installed", func(t *testing.T) {
		// The fix must not turn the real "no Chrome" verdict into an error.
		o := &browserSetupOpener{
			goos:       "darwin",
			findChrome: func(context.Context) (string, error) { return "", nil },
			start:      func(context.Context, []string) error { return nil },
		}
		res, berr := o.open(context.Background(), browserSetupChromeRemoteDebugging)
		if berr != nil {
			t.Fatalf("an evaluated absence is a verdict, not an error: %v", berr)
		}
		if res["outcome"] != browserSetupOutcomeChromeNotInstalled {
			t.Fatalf("expected %q, got %v", browserSetupOutcomeChromeNotInstalled, res["outcome"])
		}
	})
}
