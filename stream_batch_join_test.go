package main

import "testing"

func TestJoinStreamBatch(t *testing.T) {
	cases := []struct {
		name    string
		entries []streamBatchEntry
		want    string
	}{
		{
			name: "adjacent deltas join with no separator",
			entries: []streamBatchEntry{
				{text: "IMPLEMENT", fragment: true},
				{text: "ATION COMPLETE", fragment: true},
				{text: "\n\nTESTS:\n- Counts: New 0,", fragment: true},
				{text: " Updated 2", fragment: true},
			},
			want: "IMPLEMENTATION COMPLETE\n\nTESTS:\n- Counts: New 0, Updated 2",
		},
		{
			name: "whole lines keep their newline",
			entries: []streamBatchEntry{
				{text: "line one"},
				{text: "line two"},
			},
			want: "line one\nline two",
		},
		{
			name: "a line next to a fragment is still separated",
			entries: []streamBatchEntry{
				{text: "Please run /login", fragment: false},
				{text: "Hel", fragment: true},
				{text: "lo", fragment: true},
				{text: "stderr: warning", fragment: false},
			},
			want: "Please run /login\nHello\nstderr: warning",
		},
		{
			name:    "empty batch",
			entries: nil,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinStreamBatch(tc.entries); got != tc.want {
				t.Fatalf("joinStreamBatch() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Claude's stream-json deltas split words at arbitrary points. Rendering each
// frame and joining the batch must reproduce the text exactly.
func TestClaudeDeltaFramesJoinWithoutSplittingWords(t *testing.T) {
	frames := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"IMPLEMENT"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ATION COMPLETE\n\nTESTS:\n-"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Counts: New 0,"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\n Updated 2"}}}`,
	}
	var batch []streamBatchEntry
	for _, frame := range frames {
		text := extractDisplayText("claude", frame)
		if text == "" {
			t.Fatalf("frame rendered empty: %s", frame)
		}
		batch = append(batch, streamBatchEntry{text: text, fragment: isClaudeStructuredStreamLine(frame)})
	}
	want := "IMPLEMENTATION COMPLETE\n\nTESTS:\n- Counts: New 0,\n Updated 2"
	if got := joinStreamBatch(batch); got != want {
		t.Fatalf("joined = %q, want %q", got, want)
	}
}

// A thinking block followed by the answer must not be concatenated: both are
// fragments, and the frames that close one block and open the next render no
// text at all.
func TestClaudeThinkingAndTextBlocksStaySeparated(t *testing.T) {
	frames := []string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"considering"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Final"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":" answer"}}}`,
	}
	var tracker claudeBlockTracker
	var batch []streamBatchEntry
	for _, frame := range frames {
		displayText := extractDisplayText("claude", frame)
		if displayText == "" {
			continue // lifecycle frames render nothing
		}
		displayText = tracker.separate(frame, displayText, batch)
		batch = append(batch, streamBatchEntry{text: displayText, fragment: isClaudeStructuredStreamLine(frame)})
	}
	want := "\n--- Thinking ---\nconsidering\nFinal answer"
	if got := joinStreamBatch(batch); got != want {
		t.Fatalf("joined = %q, want %q", got, want)
	}
}

// Within one block the tracker must stay out of the way — deltas still join
// with nothing between them, so an exact marker survives the frame boundary.
func TestClaudeBlockTrackerLeavesIntraBlockDeltasAlone(t *testing.T) {
	frames := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"IMPLEMENT"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ATION COMPLETE"}}}`,
	}
	var tracker claudeBlockTracker
	var batch []streamBatchEntry
	for _, frame := range frames {
		displayText := tracker.separate(frame, extractDisplayText("claude", frame), batch)
		batch = append(batch, streamBatchEntry{text: displayText, fragment: true})
	}
	if got := joinStreamBatch(batch); got != "IMPLEMENTATION COMPLETE" {
		t.Fatalf("joined = %q, want %q", got, "IMPLEMENTATION COMPLETE")
	}
}

// A tool_use block already renders its own leading newline; the boundary must
// not add a second one.
func TestClaudeBlockBoundaryDoesNotDoubleExistingNewline(t *testing.T) {
	frames := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Reading the file."}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","name":"Read"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"Done."}}}`,
	}
	var tracker claudeBlockTracker
	var batch []streamBatchEntry
	for _, frame := range frames {
		displayText := tracker.separate(frame, extractDisplayText("claude", frame), batch)
		batch = append(batch, streamBatchEntry{text: displayText, fragment: true})
	}
	want := "Reading the file.\n[Using tool: Read]\nDone."
	if got := joinStreamBatch(batch); got != want {
		t.Fatalf("joined = %q, want %q", got, want)
	}
}
