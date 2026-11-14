package main

// Grok reply chunks merged on the device (coding-agent parity plan, W2.1).
//
// Grok streams a reply as many `session/update` notifications whose update is
// an `agent_message_chunk`, and readStream publishes every stdout frame
// through ONE ordered publisher that waits for each Pub/Sub ack (~20 ms). The
// `session/prompt` answer that ends a turn carries only its stopReason (the
// reply exists only in the chunks before it), so it queued behind every one
// of them. The 12 most recent Grok sessions on one prod-channel computer
// published 147 to 2,267 stdout frames each (its agent.log, 2026-11-13).
// Claude's fix, the ordered-first publisher (#289), cannot apply: deferring
// the chunks would put the answer ahead of the text it ends.
//
// So consecutive message chunks of one prompt are merged into ONE frame
// before they are queued, by the rule the one-shot core already uses for its
// deltas (oneShotDeltaFlushBytes / oneShotDeltaFlushDelay): a merged frame
// holds at most 4 KiB of text and waits at most 250 ms. Any other frame
// flushes the merged text first and keeps its place behind it: a tool call
// (where one message ends and the next begins), a permission request, a
// thought chunk, the prompt answer, a stderr line, an error. Thought text
// is never merged into message text.
//
// The merged frame is still one valid ACP notification: the first chunk's
// frame carrying the joined text. That is a deliberate exception to "we never
// edit grok's wire format" (readStream). Grok's notification-level `_meta`
// changes with every chunk (`chunkId`, `eventId`, `agentTimestampMs`), so
// those fields are not compared and the later chunks' values are dropped: no
// reader reads them (shared-resources parseGrokChunk, the frontend's Grok
// normalizer, ai-service's grokSession, voice-service and terminal-service
// read the update's text and kind). Chunks merge only within one `promptId`
// and when everything else in the frame is equal. A frame published after
// merging takes ONE seq, so the session's seqs stay contiguous: voice-service
// holds a Grok chunk read above a gap in them for up to a second.
//
// A chunk of 4 KiB or more, and a chunk that is merged with nothing, are
// published exactly as Grok wrote them.

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// grokChunkMergeDelay is how long merged text may wait for more. A var so
// tests can hold a merge open while they write a burst.
var grokChunkMergeDelay = oneShotDeltaFlushDelay

// grokChunkMergeBytes caps the text one merged frame carries.
const grokChunkMergeBytes = oneShotDeltaFlushBytes

// grokMessageChunk is one stdout frame the merger may join with its
// neighbours: a `session/update` notification whose update is an
// `agent_message_chunk` with plain text content.
type grokMessageChunk struct {
	line  string
	frame map[string]any
	text  string
	// key is what two chunks must share to merge: the whole frame except the
	// text and the notification `_meta`, plus that `_meta`'s promptId.
	key string
}

// parseGrokMessageChunk returns the frame as a mergeable chunk, or false for
// every other frame (published on its own, after any merged text).
func parseGrokMessageChunk(line string) (grokMessageChunk, bool) {
	if !strings.Contains(line, `"agent_message_chunk"`) {
		return grokMessageChunk{}, false
	}
	dec := json.NewDecoder(strings.NewReader(line))
	// Numbers stay exactly as written (a 13-digit timestamp, a large id).
	dec.UseNumber()
	var frame map[string]any
	if err := dec.Decode(&frame); err != nil || dec.More() {
		return grokMessageChunk{}, false
	}
	if _, hasID := frame["id"]; hasID || frame["method"] != "session/update" {
		return grokMessageChunk{}, false
	}
	params, ok := frame["params"].(map[string]any)
	if !ok {
		return grokMessageChunk{}, false
	}
	update, ok := params["update"].(map[string]any)
	if !ok || update["sessionUpdate"] != "agent_message_chunk" {
		return grokMessageChunk{}, false
	}
	content, ok := update["content"].(map[string]any)
	if !ok || content["type"] != "text" {
		return grokMessageChunk{}, false
	}
	text, ok := content["text"].(string)
	if !ok {
		return grokMessageChunk{}, false
	}

	// The key: the frame without the text, and with the notification `_meta`
	// reduced to its promptId. The frame itself is left untouched.
	keyContent := make(map[string]any, len(content))
	for k, v := range content {
		if k != "text" {
			keyContent[k] = v
		}
	}
	keyUpdate := make(map[string]any, len(update))
	for k, v := range update {
		keyUpdate[k] = v
	}
	keyUpdate["content"] = keyContent
	keyParams := make(map[string]any, len(params))
	for k, v := range params {
		keyParams[k] = v
	}
	keyParams["update"] = keyUpdate
	if meta, present := params["_meta"]; present {
		metaMap, ok := meta.(map[string]any)
		if !ok {
			return grokMessageChunk{}, false
		}
		keyParams["_meta"] = map[string]any{"promptId": metaMap["promptId"]}
	}
	keyFrame := make(map[string]any, len(frame))
	for k, v := range frame {
		keyFrame[k] = v
	}
	keyFrame["params"] = keyParams
	key, err := json.Marshal(keyFrame)
	if err != nil {
		return grokMessageChunk{}, false
	}
	return grokMessageChunk{line: line, frame: frame, text: text, key: string(key)}, true
}

// renderMergedGrokChunk is first's frame carrying text, as one JSON line.
// HTML characters are left as Grok wrote them; key order follows Go's
// (sorted), which no JSON reader depends on.
func renderMergedGrokChunk(first grokMessageChunk, text string) (string, bool) {
	content := first.frame["params"].(map[string]any)["update"].(map[string]any)["content"].(map[string]any)
	prev := content["text"]
	content["text"] = text
	defer func() { content["text"] = prev }()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(first.frame); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

// grokChunkMerger holds the message chunks not yet published. Every frame of
// a session, merged or not, is published through it under one mutex, so a
// timer flush and the readers' next frames can never reorder, and a frame's
// seq (assigned by publishLine / inside emit) follows publish order.
type grokChunkMerger struct {
	mu sync.Mutex
	// publishLine publishes one grok_acp_message frame (assigning its seq);
	// false means the session failed and nothing more may be published.
	publishLine func(line string) bool
	pending     []grokMessageChunk
	text        strings.Builder
	timer       *time.Timer
	closed      bool
	failed      bool
	// merged counts the chunks that went out inside a merged frame, and
	// frames the merged frames, for the stream reader's closing log line.
	merged, frames int
}

func newGrokChunkMerger(publishLine func(line string) bool) *grokChunkMerger {
	return &grokChunkMerger{publishLine: publishLine}
}

// add takes one message chunk. false: the session failed, stop reading.
func (c *grokChunkMerger) add(chunk grokMessageChunk) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed {
		return false
	}
	if c.closed {
		return c.publishLocked(chunk.line)
	}
	if len(c.pending) > 0 &&
		(chunk.key != c.pending[0].key || c.text.Len()+len(chunk.text) > grokChunkMergeBytes) {
		if !c.flushLocked() {
			return false
		}
	}
	if len(c.pending) == 0 && len(chunk.text) >= grokChunkMergeBytes {
		// Big enough on its own: published as Grok wrote it.
		return c.publishLocked(chunk.line)
	}
	c.pending = append(c.pending, chunk)
	c.text.WriteString(chunk.text)
	if c.text.Len() >= grokChunkMergeBytes {
		return c.flushLocked()
	}
	if c.timer == nil {
		c.timer = time.AfterFunc(grokChunkMergeDelay, c.flushOnTimer)
	}
	return true
}

// emit runs publish (one frame that is not a message chunk) after the merged
// text ahead of it. false: the session failed, stop reading.
func (c *grokChunkMerger) emit(publish func() bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failed || !c.flushLocked() {
		return false
	}
	if !publish() {
		c.failed = true
		return false
	}
	return true
}

// close publishes what is held and stops the timer. Frames still arriving
// (stderr can outlive stdout) are then published one by one, unmerged.
func (c *grokChunkMerger) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.failed {
		c.flushLocked()
	}
	c.stopTimerLocked()
	c.closed = true
}

func (c *grokChunkMerger) flushOnTimer() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.failed {
		return
	}
	c.flushLocked()
}

func (c *grokChunkMerger) stopTimerLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

func (c *grokChunkMerger) flushLocked() bool {
	c.stopTimerLocked()
	if c.failed {
		return false
	}
	if len(c.pending) == 0 {
		return true
	}
	pending := c.pending
	text := c.text.String()
	c.pending = nil
	c.text.Reset()
	if len(pending) == 1 {
		return c.publishLocked(pending[0].line)
	}
	line, ok := renderMergedGrokChunk(pending[0], text)
	if !ok {
		// Cannot happen for a frame that decoded; publish them one by one
		// rather than lose text.
		for _, p := range pending {
			if !c.publishLocked(p.line) {
				return false
			}
		}
		return true
	}
	c.merged += len(pending)
	c.frames++
	return c.publishLocked(line)
}

func (c *grokChunkMerger) publishLocked(line string) bool {
	if !c.publishLine(line) {
		c.failed = true
		return false
	}
	return true
}

// counts returns how many chunks went out merged, and in how many frames.
func (c *grokChunkMerger) counts() (merged, frames int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.merged, c.frames
}
