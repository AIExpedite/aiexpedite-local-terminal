// File: pty_normalizer_test.go
// Unit tests for the token-safe PTY output normalizer: carriage-return redraw
// collapse, ANSI/cursor stripping, consecutive-frame dedup, redraw rate
// limiting, split-escape handling, and prompt detection.
package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_700_000_000, 0)

// writeAll feeds the whole input at time t0 and flushes, returning all emitted
// lines (as they would be streamed to the model).
func writeAll(n *PTYNormalizer, input string) []string {
	out := n.Write([]byte(input), t0)
	if s, ok := n.Flush(); ok {
		out = append(out, s)
	}
	return out
}

func TestNormalizer_CRRedrawCollapsesToFinalLine(t *testing.T) {
	n := NewPTYNormalizer(0)
	// A progress bar redrawing the same line via CR, then a final newline.
	got := writeAll(n, "10%\r45%\r90%\rdone\n")
	want := []string{"done"}
	if !equalLines(got, want) {
		t.Fatalf("CR redraw: got %q want %q", got, want)
	}
	if n.Counters.FramesCollapsed == 0 {
		t.Errorf("expected FramesCollapsed > 0, got %d", n.Counters.FramesCollapsed)
	}
}

func TestNormalizer_ThousandFrameProgressBarCollapses(t *testing.T) {
	n := NewPTYNormalizer(0)
	var sb strings.Builder
	for i := 0; i < 1000; i++ {
		sb.WriteString("\r[")
		sb.WriteString(strings.Repeat("#", i/50))
		sb.WriteString("] ")
		sb.WriteString(strings.Repeat("=", i%7))
	}
	sb.WriteString("\rComplete\n")
	got := writeAll(n, sb.String())
	// A 1,000-frame progress bar must collapse to ~one useful final line.
	if len(got) != 1 || got[0] != "Complete" {
		t.Fatalf("progress bar: got %d lines %q, want [\"Complete\"]", len(got), got)
	}
}

func TestNormalizer_StripsANSIColorAndCursor(t *testing.T) {
	n := NewPTYNormalizer(0)
	// Color SGR, erase-line, cursor-home, then text.
	got := writeAll(n, "\x1b[31m\x1b[2K\x1b[Herror: \x1b[1mboom\x1b[0m\n")
	want := []string{"error: boom"}
	if !equalLines(got, want) {
		t.Fatalf("ANSI strip: got %q want %q", got, want)
	}
	if strings.ContainsRune(strings.Join(got, ""), '\x1b') {
		t.Errorf("output still contains ESC bytes: %q", got)
	}
}

func TestNormalizer_DedupesIdenticalConsecutiveFrames(t *testing.T) {
	n := NewPTYNormalizer(0)
	got := writeAll(n, "same\nsame\nsame\ndifferent\n")
	want := []string{"same", "different"}
	if !equalLines(got, want) {
		t.Fatalf("dedup: got %q want %q", got, want)
	}
	if n.Counters.FramesDeduped != 2 {
		t.Errorf("expected 2 deduped, got %d", n.Counters.FramesDeduped)
	}
}

func TestNormalizer_SplitEscapeAcrossChunks(t *testing.T) {
	n := NewPTYNormalizer(0)
	// The CSI sequence "\x1b[31m" is split across three Write calls.
	var got []string
	got = append(got, n.Write([]byte("hi \x1b["), t0)...)
	got = append(got, n.Write([]byte("31"), t0)...)
	got = append(got, n.Write([]byte("mred\n"), t0)...)
	if s, ok := n.Flush(); ok {
		got = append(got, s)
	}
	want := []string{"hi red"}
	if !equalLines(got, want) {
		t.Fatalf("split escape: got %q want %q", got, want)
	}
}

func TestNormalizer_SplitLineAcrossChunks(t *testing.T) {
	n := NewPTYNormalizer(0)
	var got []string
	got = append(got, n.Write([]byte("hello "), t0)...)
	got = append(got, n.Write([]byte("world\n"), t0)...)
	want := []string{"hello world"}
	if !equalLines(got, want) {
		t.Fatalf("split line: got %q want %q", got, want)
	}
}

func TestNormalizer_RateLimitsUnterminatedRedraw(t *testing.T) {
	n := NewPTYNormalizer(200 * time.Millisecond)
	now := t0
	emitted := 0
	// 50 CR-redraws of an un-terminated line, ticking 20ms between each. With a
	// 200ms redraw interval only a handful of intermediate frames should escape.
	for i := 0; i < 50; i++ {
		n.Write([]byte("\rprogress "+strings.Repeat(".", i%5)), now)
		if _, ok := n.MaybeFlushRedraw(now); ok {
			emitted++
		}
		now = now.Add(20 * time.Millisecond)
	}
	if emitted > 6 {
		t.Fatalf("rate limit: emitted %d intermediate frames, want <= 6", emitted)
	}
	if n.Counters.RedrawsRateLimited == 0 {
		t.Errorf("expected some redraws rate-limited, got 0")
	}
}

// A single un-terminated line-run that redraws for a LONG time (ticking well
// past the redraw interval on every frame) must still emit at most
// maxRedrawEmitsPerRun intermediate frames — token cost is bounded by frame
// COUNT, not duration. Then the terminating newline yields exactly one final
// useful line. This is the real-world 1,000-frame progress-bar case that a
// pure interval limiter would leak dozens of lines for.
func TestNormalizer_LongRedrawRunCappedByFrameBudget(t *testing.T) {
	n := NewPTYNormalizer(10 * time.Millisecond)
	now := t0
	emitted := 0
	// 500 distinct redraw frames, each a full interval apart (so the interval
	// limiter alone would let ALL of them through).
	for i := 0; i < 500; i++ {
		n.Write([]byte(fmt.Sprintf("\rprogress %d%%", i)), now)
		if _, ok := n.MaybeFlushRedraw(now); ok {
			emitted++
		}
		now = now.Add(50 * time.Millisecond)
	}
	if emitted > maxRedrawEmitsPerRun {
		t.Fatalf("frame budget: emitted %d intermediate frames over the run, want <= %d",
			emitted, maxRedrawEmitsPerRun)
	}
	// Terminating the line resets the budget and always emits the final state.
	final := n.Write([]byte("progress 100%\n"), now)
	if len(final) != 1 || !strings.Contains(final[0], "100%") {
		t.Fatalf("final terminated line: got %q, want one line containing 100%%", final)
	}
}

// The per-run budget resets on newline, so a multi-line log keeps per-line
// liveness rather than being permanently muted after the first busy line.
func TestNormalizer_RedrawBudgetResetsPerLine(t *testing.T) {
	n := NewPTYNormalizer(10 * time.Millisecond)
	now := t0
	total := 0
	for line := 0; line < 3; line++ {
		for i := 0; i < 5; i++ {
			n.Write([]byte(fmt.Sprintf("\rline%d step %d", line, i)), now)
			if _, ok := n.MaybeFlushRedraw(now); ok {
				total++
			}
			now = now.Add(50 * time.Millisecond)
		}
		n.Write([]byte("\n"), now) // terminate → resets the per-run budget
	}
	// Up to maxRedrawEmitsPerRun intermediate frames per line-run across 3 runs.
	if total == 0 || total > 3*maxRedrawEmitsPerRun {
		t.Fatalf("per-line budget: emitted %d intermediate frames, want 1..=%d", total, 3*maxRedrawEmitsPerRun)
	}
}

func TestNormalizer_MergedStreamStaysLineOriented(t *testing.T) {
	n := NewPTYNormalizer(0)
	// PTY merges stdout+stderr; interleaved lines still render one-per-line.
	got := writeAll(n, "stdout line\nstderr line\nstdout again\n")
	want := []string{"stdout line", "stderr line", "stdout again"}
	if !equalLines(got, want) {
		t.Fatalf("merged stream: got %q want %q", got, want)
	}
}

func TestNormalizer_PendingPromptDetection(t *testing.T) {
	cases := []struct {
		in     string
		prompt bool
	}{
		{"Password: ", true},
		{"Username for 'https://github.com': ", true},
		{"Are you sure you want to continue connecting (yes/no)? ", true},
		{"Overwrite? [y/N] ", true},
		{"Enter passphrase for key '/root/.ssh/id_rsa': ", true},
		{"Building project...", false},
		{"", false},
	}
	for _, c := range cases {
		n := NewPTYNormalizer(0)
		n.Write([]byte(c.in), t0) // no newline: prompt sits un-emitted on the line
		_, got := n.PendingPromptLine()
		if got != c.prompt {
			t.Errorf("PendingPromptLine(%q) = %v, want %v", c.in, got, c.prompt)
		}
	}
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
