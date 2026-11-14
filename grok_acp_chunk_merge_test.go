package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// grokChunkFrame is one `agent_message_chunk` notification shaped like Grok's
// own (a captured update's keys: a notification `_meta` whose chunkId,
// eventId and agentTimestampMs change per chunk, and whose promptId does not).
func grokChunkFrame(kind, promptID string, n int, text string) string {
	encoded, _ := json.Marshal(text)
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_1","update":{"sessionUpdate":%q,"content":{"type":"text","text":%s}},"_meta":{"agentTimestampMs":%d,"chunkId":%d,"eventId":"ev-%d","promptId":%q,"totalTokens":%d,"updateType":"agentMessageChunk"}}}`,
		kind, encoded, 1784000000000+int64(n), n, n, promptID, 100+n)
}

func grokMessageChunkFrame(n int, text string) string {
	return grokChunkFrame("agent_message_chunk", "prompt-1", n, text)
}

const (
	grokPromptAnswerFrame = `{"jsonrpc":"2.0","id":7,"result":{"stopReason":"end_turn"}}`
	grokToolCallFrame     = `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess_1","update":{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Read file","kind":"read","status":"pending"}}}`
)

// grokStreamHarness runs readStream over pipes and records what it publishes.
type grokStreamHarness struct {
	t       *testing.T
	session *GrokACPSession
	stdout  *io.PipeWriter
	stderr  *io.PipeWriter
	mu      sync.Mutex
	msgs    []resultMsg
}

func startGrokStreamHarness(t *testing.T, mergeDelay time.Duration) *grokStreamHarness {
	t.Helper()
	prev := grokChunkMergeDelay
	grokChunkMergeDelay = mergeDelay
	t.Cleanup(func() { grokChunkMergeDelay = prev })

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	h := &grokStreamHarness{t: t, stdout: stdoutW, stderr: stderrW}
	h.session = &GrokACPSession{
		ID:          "grok-merge-1",
		WorkspaceID: "ws",
		UID:         "uid",
		status:      "running",
		done:        make(chan struct{}),
		streamDone:  make(chan struct{}),
		firstFrame:  make(chan struct{}),
		Stdout:      stdoutR,
		Stderr:      stderrR,
	}
	go NewGrokACPManager(nil).readStream(h.session, func(msg resultMsg) {
		h.mu.Lock()
		h.msgs = append(h.msgs, msg)
		h.mu.Unlock()
	})
	t.Cleanup(func() { h.finish() })
	return h
}

// writeStdout writes lines in ONE write, so the reader sees them as a burst.
func (h *grokStreamHarness) writeStdout(lines ...string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdout, strings.Join(lines, "\n")+"\n"); err != nil {
		h.t.Fatalf("write stdout: %v", err)
	}
}

func (h *grokStreamHarness) writeStderr(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stderr, line+"\n"); err != nil {
		h.t.Fatalf("write stderr: %v", err)
	}
}

// finish closes both pipes and waits for readStream to return, so every
// frame it will publish has been published.
func (h *grokStreamHarness) finish() []resultMsg {
	_ = h.stdout.Close()
	_ = h.stderr.Close()
	select {
	case <-h.session.streamDone:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("readStream did not return")
	}
	return h.published()
}

func (h *grokStreamHarness) published() []resultMsg {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]resultMsg(nil), h.msgs...)
}

// waitFor returns once n frames are published.
func (h *grokStreamHarness) waitFor(n int) []resultMsg {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.published(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("expected %d frames, got %s", n, grokFramesDump(h.published()))
	return nil
}

// grokFrameText reads a published frame the way shared-resources
// parseGrokChunk does: the update's kind and its text.
func grokFrameText(t *testing.T, msg resultMsg) (kind, text string) {
	t.Helper()
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Content       struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"update"`
		} `json:"params"`
		Result *struct {
			StopReason string `json:"stopReason"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(msg.Output), &frame); err != nil {
		t.Fatalf("a published frame is not JSON: %v: %s", err, msg.Output)
	}
	if frame.Result != nil {
		return "answer", ""
	}
	return frame.Params.Update.SessionUpdate, frame.Params.Update.Content.Text
}

func grokFrameKinds(t *testing.T, msgs []resultMsg) []string {
	t.Helper()
	var out []string
	for _, m := range msgs {
		if m.Type != "grok_acp_message" {
			out = append(out, m.Type)
			continue
		}
		kind, _ := grokFrameText(t, m)
		out = append(out, kind)
	}
	return out
}

func grokFramesDump(msgs []resultMsg) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "\n  seq=%d %s %s", m.Seq, m.Type, truncateString(m.Output, 160))
	}
	return b.String()
}

// assertGrokSeqsContiguous: voice-service holds a Grok chunk read above a gap
// in the session's seqs, so a merged frame must never leave one.
func assertGrokSeqsContiguous(t *testing.T, msgs []resultMsg) {
	t.Helper()
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Fatalf("seqs must run 1..n in publish order with no gap:%s", grokFramesDump(msgs))
		}
	}
}

func TestGrokChunkMerge_BurstPublishesAsOneFrameWithTheJoinedText(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	var lines []string
	var want strings.Builder
	for i := 1; i <= 50; i++ {
		piece := fmt.Sprintf("word%02d ", i)
		want.WriteString(piece)
		lines = append(lines, grokMessageChunkFrame(i, piece))
	}
	lines = append(lines, grokPromptAnswerFrame)
	h.writeStdout(lines...)
	msgs := h.waitFor(2)
	h.finish()

	if kinds := grokFrameKinds(t, msgs); strings.Join(kinds, ",") != "agent_message_chunk,answer" {
		t.Fatalf("50 chunks and the answer must publish as 2 frames, got %v:%s", kinds, grokFramesDump(msgs))
	}
	if _, text := grokFrameText(t, msgs[0]); text != want.String() {
		t.Fatalf("the merged frame must carry every chunk's text in order:\n got %q\nwant %q", text, want.String())
	}
	assertGrokSeqsContiguous(t, msgs)
}

func TestGrokChunkMerge_ToolCallFlushesTheTextBeforeIt(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	h.writeStdout(
		grokMessageChunkFrame(1, "I will "),
		grokMessageChunkFrame(2, "read it."),
		grokToolCallFrame,
		grokMessageChunkFrame(3, "It says "),
		grokMessageChunkFrame(4, "hello."),
		grokPromptAnswerFrame,
	)
	msgs := h.waitFor(4)
	h.finish()

	kinds := grokFrameKinds(t, msgs)
	if strings.Join(kinds, ",") != "agent_message_chunk,tool_call,agent_message_chunk,answer" {
		t.Fatalf("a tool call must split the merge and keep its place, got %v:%s", kinds, grokFramesDump(msgs))
	}
	if _, text := grokFrameText(t, msgs[0]); text != "I will read it." {
		t.Fatalf("text before the tool call: %q", text)
	}
	if _, text := grokFrameText(t, msgs[2]); text != "It says hello." {
		t.Fatalf("text after the tool call: %q", text)
	}
	assertGrokSeqsContiguous(t, msgs)
}

func TestGrokChunkMerge_PromptAnswerNeverOvertakesTheTextBeforeIt(t *testing.T) {
	// Each chunk on its own write, the answer right behind the last: whatever
	// the scheduling, every published text frame precedes the answer and the
	// answer's seq is the last.
	h := startGrokStreamHarness(t, 10*time.Second)
	for i := 1; i <= 20; i++ {
		h.writeStdout(grokMessageChunkFrame(i, "x"))
	}
	h.writeStdout(grokPromptAnswerFrame)
	msgs := h.finish()

	if len(msgs) == 0 {
		t.Fatal("nothing published")
	}
	last := msgs[len(msgs)-1]
	if kind, _ := grokFrameText(t, last); kind != "answer" {
		t.Fatalf("the answer must be published last:%s", grokFramesDump(msgs))
	}
	var text strings.Builder
	for _, m := range msgs[:len(msgs)-1] {
		_, piece := grokFrameText(t, m)
		text.WriteString(piece)
	}
	if text.String() != strings.Repeat("x", 20) {
		t.Fatalf("every chunk's text must be published before the answer, got %q", text.String())
	}
	assertGrokSeqsContiguous(t, msgs)
}

func TestGrokChunkMerge_ThoughtTextIsNeverMergedIntoMessageText(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	thought := grokChunkFrame("agent_thought_chunk", "prompt-1", 2, "thinking...")
	h.writeStdout(
		grokMessageChunkFrame(1, "Hello"),
		thought,
		grokMessageChunkFrame(3, " world"),
		grokPromptAnswerFrame,
	)
	msgs := h.waitFor(4)
	h.finish()

	kinds := grokFrameKinds(t, msgs)
	if strings.Join(kinds, ",") != "agent_message_chunk,agent_thought_chunk,agent_message_chunk,answer" {
		t.Fatalf("a thought chunk must stay its own frame, got %v:%s", kinds, grokFramesDump(msgs))
	}
	if msgs[1].Output != thought {
		t.Fatalf("the thought frame must be published as Grok wrote it:\n got %s\nwant %s", msgs[1].Output, thought)
	}
	for _, m := range msgs {
		if kind, text := grokFrameText(t, m); kind == "agent_message_chunk" && strings.Contains(text, "thinking") {
			t.Fatalf("thought text leaked into message text: %q", text)
		}
	}
}

func TestGrokChunkMerge_MergedFrameIsTheFirstChunksNotification(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	// 2^53 + 1: a float64 cannot hold it, so it survives only if the merge
	// keeps numbers exactly as Grok wrote them.
	first := strings.Replace(grokMessageChunkFrame(1, "a<b>&c "), `"chunkId":1,`, `"chunkId":9007199254740993,`, 1)
	h.writeStdout(first, grokMessageChunkFrame(2, "d"), grokPromptAnswerFrame)
	msgs := h.waitFor(2)
	h.finish()

	var got, want map[string]any
	if err := json.Unmarshal([]byte(msgs[0].Output), &got); err != nil {
		t.Fatalf("merged frame: %v", err)
	}
	if err := json.Unmarshal([]byte(first), &want); err != nil {
		t.Fatal(err)
	}
	want["params"].(map[string]any)["update"].(map[string]any)["content"].(map[string]any)["text"] = "a<b>&c d"
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("the merged frame must be the first chunk's notification with the joined text:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	// Go's default encoder writes '<' as a backslash-u escape; the merged
	// frame keeps it as Grok wrote it.
	htmlEscaped := `\` + "u003c"
	if strings.Contains(msgs[0].Output, htmlEscaped) || !strings.Contains(msgs[0].Output, "a<b>&c d") {
		t.Fatalf("text must not be HTML-escaped: %s", msgs[0].Output)
	}
	if !strings.Contains(msgs[0].Output, `"agentTimestampMs":1784000000001`) ||
		!strings.Contains(msgs[0].Output, `"chunkId":9007199254740993`) {
		t.Fatalf("numbers must keep their exact digits: %s", msgs[0].Output)
	}
}

func TestGrokChunkMerge_ALoneChunkIsPublishedAsGrokWroteIt(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	only := grokMessageChunkFrame(1, "just this")
	h.writeStdout(only, grokPromptAnswerFrame)
	msgs := h.waitFor(2)
	h.finish()
	if msgs[0].Output != only {
		t.Fatalf("a chunk merged with nothing must be published verbatim:\n got %s\nwant %s", msgs[0].Output, only)
	}
}

func TestGrokChunkMerge_DifferentPromptsAreNotMerged(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	h.writeStdout(
		grokChunkFrame("agent_message_chunk", "prompt-1", 1, "first "),
		grokChunkFrame("agent_message_chunk", "prompt-2", 2, "second"),
		grokPromptAnswerFrame,
	)
	msgs := h.waitFor(3)
	h.finish()
	if kinds := grokFrameKinds(t, msgs); strings.Join(kinds, ",") != "agent_message_chunk,agent_message_chunk,answer" {
		t.Fatalf("chunks of two prompts must not merge, got %v:%s", kinds, grokFramesDump(msgs))
	}
}

func TestGrokChunkMerge_TextIsCappedAt4KiBPerFrame(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	piece := strings.Repeat("y", 1000)
	var lines []string
	for i := 1; i <= 9; i++ {
		lines = append(lines, grokMessageChunkFrame(i, piece))
	}
	big := grokMessageChunkFrame(10, strings.Repeat("z", grokChunkMergeBytes))
	lines = append(lines, big, grokPromptAnswerFrame)
	h.writeStdout(lines...)
	msgs := h.finish()

	var total strings.Builder
	sawBig := false
	for _, m := range msgs {
		kind, text := grokFrameText(t, m)
		if kind != "agent_message_chunk" {
			continue
		}
		if m.Output == big {
			sawBig = true
		} else if len(text) > grokChunkMergeBytes {
			t.Fatalf("a merged frame carries %d bytes of text, over the %d cap", len(text), grokChunkMergeBytes)
		}
		total.WriteString(text)
	}
	if !sawBig {
		t.Fatalf("a chunk at the cap must be published as Grok wrote it:%s", grokFramesDump(msgs))
	}
	if total.String() != strings.Repeat(piece, 9)+strings.Repeat("z", grokChunkMergeBytes) {
		t.Fatalf("text lost or reordered across capped frames")
	}
	assertGrokSeqsContiguous(t, msgs)
}

func TestGrokChunkMerge_HeldTextIsPublishedAfterTheDelay(t *testing.T) {
	h := startGrokStreamHarness(t, 20*time.Millisecond)
	h.writeStdout(grokMessageChunkFrame(1, "still "), grokMessageChunkFrame(2, "typing"))
	msgs := h.waitFor(1)
	if _, text := grokFrameText(t, msgs[0]); text != "still typing" {
		t.Fatalf("held text must go out after the delay with nothing behind it, got %q", text)
	}
	h.finish()
}

func TestGrokChunkMerge_StderrLineWaitsForTheTextReadBeforeIt(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	h.writeStdout(grokMessageChunkFrame(1, "before "), grokMessageChunkFrame(2, "stderr"))
	// The chunks are held; wait until the stdout reader has taken them in.
	time.Sleep(100 * time.Millisecond)
	h.writeStderr("grok: a diagnostic line")
	msgs := h.waitFor(2)
	h.finish()
	if msgs[0].Type != "grok_acp_message" || msgs[1].Type != "grok_acp_stderr" {
		t.Fatalf("the stderr line must follow the reply text read before it:%s", grokFramesDump(msgs))
	}
	assertGrokSeqsContiguous(t, msgs)
}

func TestGrokChunkMerge_EndOfStreamPublishesHeldText(t *testing.T) {
	h := startGrokStreamHarness(t, 10*time.Second)
	h.writeStdout(grokMessageChunkFrame(1, "last "), grokMessageChunkFrame(2, "words"))
	msgs := h.finish()
	if len(msgs) != 1 {
		t.Fatalf("held text must be published when stdout ends:%s", grokFramesDump(msgs))
	}
	if _, text := grokFrameText(t, msgs[0]); text != "last words" {
		t.Fatalf("got %q", text)
	}
}

func TestParseGrokMessageChunk_OnlyPlainTextMessageChunks(t *testing.T) {
	for name, line := range map[string]string{
		"thought": grokChunkFrame("agent_thought_chunk", "p", 1, "x"),
		// Passes the cheap text pre-filter, so only the kind check refuses it.
		"thought naming the kind": strings.Replace(grokChunkFrame("agent_thought_chunk", "p", 1, "x"),
			`"updateType":"agentMessageChunk"`, `"updateType":"agent_message_chunk"`, 1),
		"request":         `{"jsonrpc":"2.0","id":3,"method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}`,
		"image content":   `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"x"}}}}`,
		"non-object meta": `{"jsonrpc":"2.0","method":"session/update","params":{"_meta":"x","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}}`,
		"answer":          grokPromptAnswerFrame,
		"tool call":       grokToolCallFrame,
	} {
		if _, ok := parseGrokMessageChunk(line); ok {
			t.Errorf("%s: must not be merged", name)
		}
	}
	if _, ok := parseGrokMessageChunk(grokMessageChunkFrame(1, "x")); !ok {
		t.Error("a plain text message chunk must be mergeable")
	}
}
