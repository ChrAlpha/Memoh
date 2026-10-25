package timeline

import (
	"reflect"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

func textSegment(id string, atMs int64, text string) RenderedSegment {
	return RenderedSegment{
		MessageID:    id,
		ReceivedAtMs: atMs,
		Content:      []RenderedContentPiece{{Type: "text", Text: text}},
	}
}

func assistantTR(atMs int64, text string) TurnResponseEntry {
	return TurnResponseEntry{RequestedAtMs: atMs, Role: "assistant", Content: text}
}

func messageTexts(messages []ContextMessage) []string {
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		texts = append(texts, m.Role+":"+m.Content)
	}
	return texts
}

func omittedThrough(firstMs, lastMs int64, count int64) checkpoint.Omitted {
	return checkpoint.Omitted{Count: count, First: time.UnixMilli(firstMs), Last: time.UnixMilli(lastMs)}
}

// A session without a checkpoint composes its whole history, inputs and
// responses by time, with nothing pinned.
func TestComposeContextWithoutCheckpointMergesTheWholeHistory(t *testing.T) {
	rc := RenderedContext{textSegment("m1", 1000, "hello"), textSegment("m2", 2000, "world")}
	trs := []TurnResponseEntry{assistantTR(1500, "reply")}

	composed := ComposeContextWithCheckpoint(rc, trs, HistoryCheckpoint{})
	if composed == nil {
		t.Fatal("expected a composed result")
	}
	if got, want := messageTexts(composed.Messages), []string{"user:hello", "assistant:reply", "user:world"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("composed %q, want %q", got, want)
	}
	if !reflect.DeepEqual(composed.Messages, MergeContext(rc, trs)) {
		t.Fatalf("composition without a checkpoint diverged from the merge: %+v", composed.Messages)
	}
	for _, message := range composed.Messages {
		if message.Pinned {
			t.Fatalf("nothing is pinned without a checkpoint: %+v", message)
		}
	}
}

// The checkpoint opens the history: the notice of what it omits, then what
// it holds in its order, pinned (a summary, a kept input as its segment, a
// kept response), and only then what came after it, by time. A kept input
// renders once, in its place in the head.
func TestComposeContextWithCheckpointPinsWhatItHolds(t *testing.T) {
	kept := assistantTR(1500, "kept reply")
	kept.SourceMessageID = "h-kept"
	tail := assistantTR(4000, "tail reply")
	tail.SourceMessageID = "h-tail"
	held := HistoryCheckpoint{
		Position: messagepkg.HistoryPosition{Turn: 3, Seq: 2},
		Omitted:  omittedThrough(100, 400, 4),
		Items: []CheckpointItem{
			{SummaryID: "s1", Summary: "  earlier work  "},
			{MessageID: "m-input"},
			{Response: &kept},
			{SummaryID: "s2", Summary: "later work"},
		},
		Messages: map[string]CheckpointMessage{"m-input": {Kept: true}, "m-new": {Kept: true}},
	}
	rc := RenderedContext{textSegment("m-input", 1000, "kept input"), textSegment("m-new", 3000, "new input")}

	composed := ComposeContextWithCheckpoint(rc, []TurnResponseEntry{tail}, held)
	if composed == nil {
		t.Fatal("expected a composed result")
	}
	want := []string{
		"system:" + HistoryNotice(held.Omitted),
		"user:<summary>\nearlier work\n</summary>",
		"user:kept input",
		"assistant:kept reply",
		"user:<summary>\nlater work\n</summary>",
		"user:new input",
		"assistant:tail reply",
	}
	if got := messageTexts(composed.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("composed %q, want %q", got, want)
	}
	for i, message := range composed.Messages {
		if message.Pinned != (i < 5) {
			t.Fatalf("message %d %q pinned = %v", i, message.Content, message.Pinned)
		}
	}
	if composed.Messages[1].CompactionArtifactID != "s1" || composed.Messages[4].CompactionArtifactID != "s2" {
		t.Fatalf("summaries lost their ids: %+v", composed.Messages)
	}
	if source := composed.Messages[2].Source; source == nil || source.Kind != "external" || source.ID != "m-input" || source.Current {
		t.Fatalf("kept input source = %+v, want the external message, not current", source)
	}
	if source := composed.Messages[3].Source; source == nil || source.Kind != "history" || source.ID != "h-kept" {
		t.Fatalf("kept response source = %+v, want its history row", source)
	}
	if source := composed.Messages[5].Source; source == nil || source.ID != "m-new" || !source.Current {
		t.Fatalf("newest input source = %+v, want it current", source)
	}
}

// What the checkpoint does not hold composes after it when the checkpoint
// leaves it raw: an input with context rows when one of them is after the
// checkpoint or kept, one without context rows unless it was received by
// the last message the checkpoint omits. Whatever its time, it renders after
// the head.
func TestComposeContextWithCheckpointComposesWhatItLeavesRaw(t *testing.T) {
	rc := RenderedContext{
		textSegment("m-chatter-early", 2000, "early chatter"),
		textSegment("m-folded", 2500, "folded input"),
		textSegment("m-chatter-late", 2600, "late chatter"),
		textSegment("m-raw", 3000, "raw input"),
	}
	classified := map[string]CheckpointMessage{"m-folded": {}, "m-raw": {Kept: true}}
	summary := []CheckpointItem{{SummaryID: "s1", Summary: "folded"}}

	t.Run("omitting", func(t *testing.T) {
		held := HistoryCheckpoint{Omitted: omittedThrough(100, 2000, 2), Items: summary, Messages: classified}
		composed := ComposeContextWithCheckpoint(rc, nil, held)
		want := []string{
			"system:" + HistoryNotice(held.Omitted),
			"user:<summary>\nfolded\n</summary>",
			"user:late chatter",
			"user:raw input",
		}
		if composed == nil || !reflect.DeepEqual(messageTexts(composed.Messages), want) {
			t.Fatalf("composed %q, want %q", messageTexts(composed.Messages), want)
		}
	})
	t.Run("omitting nothing", func(t *testing.T) {
		composed := ComposeContextWithCheckpoint(rc, nil, HistoryCheckpoint{Items: summary, Messages: classified})
		want := []string{
			"user:<summary>\nfolded\n</summary>",
			"user:early chatter",
			"user:late chatter",
			"user:raw input",
		}
		if composed == nil || !reflect.DeepEqual(messageTexts(composed.Messages), want) {
			t.Fatalf("composed %q, want %q", messageTexts(composed.Messages), want)
		}
	})
}

// A summary read the rows of a message as they were written. A folded
// message changed after that, edited or deleted, composes after the head
// with what the summary never read, unless the checkpoint omits it; one
// that did not change stays folded.
func TestComposeContextWithCheckpointComposesAFoldedMessageThatChangedSince(t *testing.T) {
	changed := func(id string, atMs, cursor int64, text string) RenderedSegment {
		segment := textSegment(id, atMs, text)
		segment.LastEventCursor = cursor
		return segment
	}
	edited := textSegment("m-edited-unversioned", 2400, "edited without cursor")
	edited.EditedAtMs = 5000
	rc := RenderedContext{
		changed("m-omitted", 1500, 9, "omitted, edited"),
		changed("m-unchanged", 2000, 4, "unchanged"),
		edited,
		changed("m-edited", 2500, 8, "edited later"),
		textSegment("m-new", 3000, "new input"),
	}
	held := HistoryCheckpoint{
		Omitted: omittedThrough(1000, 1500, 1),
		Items:   []CheckpointItem{{SummaryID: "s1", Summary: "folded"}},
		Messages: map[string]CheckpointMessage{
			"m-omitted":            {EventCursor: 3},
			"m-unchanged":          {EventCursor: 4},
			"m-edited-unversioned": {},
			"m-edited":             {EventCursor: 5},
			"m-new":                {Kept: true, EventCursor: 7},
		},
	}
	composed := ComposeContextWithCheckpoint(rc, nil, held)
	want := []string{
		"system:" + HistoryNotice(held.Omitted),
		"user:<summary>\nfolded\n</summary>",
		"user:edited without cursor",
		"user:edited later",
		"user:new input",
	}
	if composed == nil || !reflect.DeepEqual(messageTexts(composed.Messages), want) {
		t.Fatalf("composed %q, want %q", messageTexts(composed.Messages), want)
	}
}

// A kept input the rendered context no longer has renders nothing; the rest
// of what the checkpoint holds keeps its order.
func TestComposeContextWithCheckpointSkipsAKeptInputItCannotRender(t *testing.T) {
	held := HistoryCheckpoint{
		Items: []CheckpointItem{
			{SummaryID: "s1", Summary: "first"},
			{MessageID: "m-gone"},
			{SummaryID: "s2", Summary: "second"},
		},
	}
	composed := ComposeContextWithCheckpoint(RenderedContext{textSegment("m-new", 3000, "new input")}, nil, held)
	want := []string{"user:<summary>\nfirst\n</summary>", "user:<summary>\nsecond\n</summary>", "user:new input"}
	if composed == nil || !reflect.DeepEqual(messageTexts(composed.Messages), want) {
		t.Fatalf("composed %q, want %q", messageTexts(composed.Messages), want)
	}
}

// Responses after the checkpoint keep the order of their time, an input
// first on the same millisecond, all after what the checkpoint holds.
func TestComposeContextWithCheckpointOrdersTheRestByTime(t *testing.T) {
	first := assistantTR(1000, "first reply")
	first.SourceMessageID = "h1"
	second := assistantTR(1000, "second reply")
	second.SourceMessageID = "h2"
	held := HistoryCheckpoint{Items: []CheckpointItem{{SummaryID: "s1", Summary: "folded"}}}

	composed := ComposeContextWithCheckpoint(RenderedContext{textSegment("m1", 1000, "input")}, []TurnResponseEntry{first, second}, held)
	want := []string{"user:<summary>\nfolded\n</summary>", "user:input", "assistant:first reply", "assistant:second reply"}
	if composed == nil || !reflect.DeepEqual(messageTexts(composed.Messages), want) {
		t.Fatalf("composed %q, want %q", messageTexts(composed.Messages), want)
	}
}

func TestComposeContextWithCheckpointEmptyInputs(t *testing.T) {
	if composed := ComposeContextWithCheckpoint(nil, nil, HistoryCheckpoint{}); composed != nil {
		t.Fatalf("expected nil result for empty inputs, got %+v", composed)
	}
	// A notice opens no history of its own.
	omitting := HistoryCheckpoint{Omitted: omittedThrough(100, 2000, 3)}
	if composed := ComposeContextWithCheckpoint(nil, nil, omitting); composed != nil {
		t.Fatalf("a checkpoint that only omits composed %q", messageTexts(composed.Messages))
	}
	if composed := ComposeContextWithCheckpoint(RenderedContext{textSegment("m0", 1500, "omitted chatter")}, nil, omitting); composed != nil {
		t.Fatalf("chatter the checkpoint omits composed %q", messageTexts(composed.Messages))
	}
	// A summary alone is history.
	summarized := HistoryCheckpoint{Omitted: omitting.Omitted, Items: []CheckpointItem{{SummaryID: "s1", Summary: "folded"}}}
	composed := ComposeContextWithCheckpoint(nil, nil, summarized)
	want := []string{"system:" + HistoryNotice(omitting.Omitted), "user:<summary>\nfolded\n</summary>"}
	if composed == nil || !reflect.DeepEqual(messageTexts(composed.Messages), want) {
		t.Fatalf("composed %q, want %q", messageTexts(composed.Messages), want)
	}
}
