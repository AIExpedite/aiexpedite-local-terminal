// File: pty_normalizer.go
// Token-safe normalization of raw PTY output before it is streamed to the
// orchestrator/model. Raw PTY bytes (which merge stdout+stderr and are full of
// ANSI color, cursor control, and carriage-return redraws) must NEVER reach the
// model — a 1,000-frame progress bar would otherwise cost thousands of tokens.
//
// The normalizer is a pure, streaming state machine:
//   - collapses carriage-return redraws to the final rendered state of a line
//   - strips ANSI color and cursor/control (CSI/OSC/ESC) sequences
//   - deduplicates identical consecutive frames/lines
//   - rate-limits intermediate redraws of an un-terminated line
//   - detects a trailing input-prompt so the caller can abort on quiet-after-prompt
//
// It keeps partial escape sequences and un-terminated lines buffered across
// Write calls, so chunk boundaries that split a sequence or a line are handled
// correctly. Time is injected (now time.Time) so the rate limiter is
// deterministic under test.
package main

import (
	"regexp"
	"strings"
	"time"
)

// DefaultRedrawInterval bounds how often an un-terminated (carriage-return only)
// line is flushed as an intermediate frame. Progress bars that redraw hundreds
// of times per second collapse to roughly one line per interval.
const DefaultRedrawInterval = 200 * time.Millisecond

// NormalizerCounters are structured metrics about how much raw terminal noise
// was collapsed. Logged (without any command content) for observability.
type NormalizerCounters struct {
	BytesIn            int
	LinesEmitted       int
	FramesCollapsed    int // carriage-return redraws collapsed within a line
	FramesDeduped      int // identical consecutive frames dropped
	RedrawsRateLimited int // intermediate redraws suppressed by the rate limiter
}

// PTYNormalizer converts raw PTY bytes into model-safe lines.
type PTYNormalizer struct {
	line      []byte // current rendered line (post CR/overwrite)
	col       int    // cursor column within line
	crPending bool   // a carriage-return was seen; next write truncates the line
	inEsc     bool   // mid ANSI escape sequence
	esc       []byte // accumulated escape bytes (for CSI/OSC terminator detection)
	escOSC    bool   // the pending escape is an OSC (terminated by BEL or ST)

	lastEmitted string
	haveLast    bool

	redrawInterval time.Duration
	lastRedrawEmit time.Time

	Counters NormalizerCounters
}

// NewPTYNormalizer creates a normalizer with the given intermediate-redraw
// interval (<=0 uses DefaultRedrawInterval).
func NewPTYNormalizer(redrawInterval time.Duration) *PTYNormalizer {
	if redrawInterval <= 0 {
		redrawInterval = DefaultRedrawInterval
	}
	return &PTYNormalizer{redrawInterval: redrawInterval}
}

// Write feeds a chunk of raw PTY bytes and returns any complete, normalized
// lines produced (without trailing newline). now drives the redraw rate limiter.
func (n *PTYNormalizer) Write(p []byte, now time.Time) []string {
	n.Counters.BytesIn += len(p)
	var out []string
	for _, b := range p {
		if n.inEsc {
			if n.consumeEscByte(b) {
				n.inEsc = false
				n.esc = n.esc[:0]
				n.escOSC = false
			}
			continue
		}
		switch b {
		case 0x1b: // ESC — begin an escape sequence
			n.inEsc = true
			n.esc = n.esc[:0]
			n.escOSC = false
		case '\r':
			n.col = 0
			if len(n.line) > 0 {
				n.crPending = true
			}
		case '\n':
			out = n.appendEmit(out, n.finalizeLine())
			n.resetLine()
		case '\b':
			if n.col > 0 {
				n.col--
			}
		case '\t':
			n.writeByte(' ')
		default:
			if b < 0x20 || b == 0x7f {
				// Strip other control characters (BEL, etc.).
				continue
			}
			n.writeByte(b)
		}
	}
	return out
}

// consumeEscByte accumulates a byte of an in-progress escape sequence and
// reports whether the sequence is now complete.
func (n *PTYNormalizer) consumeEscByte(b byte) bool {
	if len(n.esc) == 0 {
		// First byte after ESC selects the sequence family.
		n.esc = append(n.esc, b)
		switch b {
		case '[': // CSI — terminated by a final byte in 0x40..0x7e
			return false
		case ']': // OSC — terminated by BEL or ST (ESC \)
			n.escOSC = true
			return false
		default:
			// Two-byte escape (e.g. ESC =, ESC >, charset selects): done.
			return true
		}
	}
	if n.escOSC {
		if b == 0x07 { // BEL terminates OSC
			return true
		}
		if b == '\\' && len(n.esc) > 0 && n.esc[len(n.esc)-1] == 0x1b {
			return true // ST: ESC \
		}
		n.esc = append(n.esc, b)
		return false
	}
	// CSI: consume parameter/intermediate bytes until a final byte.
	if b >= 0x40 && b <= 0x7e {
		return true
	}
	n.esc = append(n.esc, b)
	return false
}

// writeByte writes a printable byte at the cursor, applying a pending
// carriage-return truncation (collapsing the prior redraw of this line).
func (n *PTYNormalizer) writeByte(b byte) {
	if n.crPending {
		n.line = n.line[:0]
		n.col = 0
		n.crPending = false
		n.Counters.FramesCollapsed++
	}
	if n.col < len(n.line) {
		n.line[n.col] = b
	} else {
		n.line = append(n.line, b)
	}
	n.col++
}

func (n *PTYNormalizer) resetLine() {
	n.line = n.line[:0]
	n.col = 0
	n.crPending = false
}

// finalizeLine renders the current line to a trimmed string.
func (n *PTYNormalizer) finalizeLine() string {
	return strings.TrimRight(string(n.line), " \t")
}

// appendEmit applies consecutive-frame dedup and appends s to out when it is
// not a duplicate of the previous emission.
func (n *PTYNormalizer) appendEmit(out []string, s string) []string {
	if n.haveLast && s == n.lastEmitted {
		n.Counters.FramesDeduped++
		return out
	}
	n.lastEmitted = s
	n.haveLast = true
	n.Counters.LinesEmitted++
	return append(out, s)
}

// MaybeFlushRedraw emits the current un-terminated line as an intermediate
// frame if the redraw interval has elapsed since the last such emission. This
// surfaces progress on a long-running carriage-return-only redraw (a bar that
// never newlines) without flooding the model. Returns the emitted line, if any.
func (n *PTYNormalizer) MaybeFlushRedraw(now time.Time) (string, bool) {
	if len(n.line) == 0 {
		return "", false
	}
	if !n.lastRedrawEmit.IsZero() && now.Sub(n.lastRedrawEmit) < n.redrawInterval {
		n.Counters.RedrawsRateLimited++
		return "", false
	}
	s := n.finalizeLine()
	if n.haveLast && s == n.lastEmitted {
		n.Counters.FramesDeduped++
		return "", false
	}
	n.lastRedrawEmit = now
	n.lastEmitted = s
	n.haveLast = true
	n.Counters.LinesEmitted++
	return s, true
}

// Flush emits any buffered, un-terminated line at end of stream.
func (n *PTYNormalizer) Flush() (string, bool) {
	if len(n.line) == 0 {
		return "", false
	}
	s := n.finalizeLine()
	n.resetLine()
	if n.haveLast && s == n.lastEmitted {
		n.Counters.FramesDeduped++
		return "", false
	}
	n.lastEmitted = s
	n.haveLast = true
	n.Counters.LinesEmitted++
	return s, true
}

// promptPatterns recognise an input-request state from the rendered tail of the
// output. Used (with an inactivity timer) to abort a tty session that emits a
// prompt and then goes quiet instead of hanging indefinitely.
var promptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)password.*:\s*$`),
	regexp.MustCompile(`(?i)passphrase.*:\s*$`),
	regexp.MustCompile(`(?i)username for .*:\s*$`),
	regexp.MustCompile(`(?i)could not read username`),
	regexp.MustCompile(`(?i)\(yes/no(?:/\[fingerprint\])?\)\??\s*$`),
	regexp.MustCompile(`(?i)\[y/n\]\s*[:?]?\s*$`),
	regexp.MustCompile(`(?i)\bare you sure\b.*\?\s*$`),
	regexp.MustCompile(`\?\s+$`), // inquirer-style trailing "? "
}

// PendingPromptLine returns the current, un-emitted rendered line if it looks
// like an input prompt (the tail is a prompt with no following output). Returns
// ("", false) otherwise. Ordinary quiet (a TUI working after emitting output)
// does not match, so it is left to the resident-session backstop.
func (n *PTYNormalizer) PendingPromptLine() (string, bool) {
	s := strings.TrimRight(string(n.line), " ")
	if s == "" {
		return "", false
	}
	if looksLikePrompt(s) {
		return strings.TrimSpace(s), true
	}
	return "", false
}

func looksLikePrompt(s string) bool {
	for _, re := range promptPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
