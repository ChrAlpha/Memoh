package timeline

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/chat/checkpoint"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// The notice is model-visible: it pins how many messages the checkpoint
// leaves out and a range of whole seconds that a search over it finds them
// all in.
func TestHistoryNoticeSaysWhatTheCheckpointLeavesOut(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*60*60)
	for _, tc := range []struct {
		name    string
		omitted checkpoint.Omitted
		want    string
	}{
		{name: "nothing omitted"},
		{
			name:    "a count without a range",
			omitted: checkpoint.Omitted{Count: 3},
		},
		{
			name: "many messages",
			omitted: checkpoint.Omitted{
				Count: 12,
				First: time.Date(2026, 8, 1, 10, 0, 0, 250e6, time.UTC),
				Last:  time.Date(2026, 8, 2, 11, 30, 0, 750e6, time.UTC),
			},
			want: "[History notice] 12 earlier messages of this conversation, sent from 2026-08-01T10:00:00Z to 2026-08-02T11:30:01Z, are not in this context. " +
				"Only its conversation summaries, if any, carry what they said; to recall details, search the message history in that time range.",
		},
		{
			name: "one message within a second",
			omitted: checkpoint.Omitted{
				Count: 1,
				First: time.Date(2026, 8, 1, 19, 0, 0, 250e6, tokyo),
				Last:  time.Date(2026, 8, 1, 19, 0, 0, 250e6, tokyo),
			},
			want: "[History notice] 1 earlier message of this conversation, sent from 2026-08-01T10:00:00Z to 2026-08-01T10:00:01Z, is not in this context. " +
				"Only its conversation summaries, if any, carry what it said; to recall details, search the message history in that time range.",
		},
		{
			name: "messages sent the same whole second",
			omitted: checkpoint.Omitted{
				Count: 2,
				First: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
				Last:  time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
			},
			want: "[History notice] 2 earlier messages of this conversation, sent at 2026-08-01T10:00:00Z, are not in this context. " +
				"Only its conversation summaries, if any, carry what they said; to recall details, search the message history in that time range.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HistoryNotice(tc.omitted); got != tc.want {
				t.Fatalf("notice = %q\nwant     %q", got, tc.want)
			}
		})
	}
}

// Composition renders nothing the checkpoint omits: the notice opens the
// history, ahead of what the checkpoint holds, and then come the inputs it
// leaves raw and the turn responses after it.
func TestComposeContextOpensWithTheHistoryNotice(t *testing.T) {
	// The checkpoint omits the three messages through the third turn, two of
	// them inputs that rendered, and holds an earlier summary.
	held := HistoryCheckpoint{
		Position: messagepkg.HistoryPosition{Turn: 3, Seq: 2},
		Omitted:  omittedThrough(1000, 2500, 3),
		Items:    []CheckpointItem{{SummaryID: "earlier", Summary: "earlier summary"}},
		Messages: map[string]CheckpointMessage{"m1": {}, "m2": {}, "m3": {Kept: true}, "m4": {Kept: true}},
	}
	rc := RenderedContext{
		textSegment("m1", 1000, "before-input-1"),
		textSegment("m2", 2000, "before-input-2"),
		textSegment("m3", 3000, "after-input-1"),
		textSegment("m4", 4000, "after-input-2"),
	}
	trs := []TurnResponseEntry{assistantTR(3500, "after-reply")}

	composed := ComposeContextWithCheckpoint(rc, trs, held)
	if composed == nil || len(composed.Messages) != 5 {
		t.Fatalf("composed = %v, want the notice, the summary and the three entries after the checkpoint", composed)
	}
	texts := messageTexts(composed.Messages)
	if texts[0] != "system:"+HistoryNotice(held.Omitted) || texts[1] != "user:<summary>\nearlier summary\n</summary>" {
		t.Fatalf("history opens with %q, want the notice and then the summary", texts[:2])
	}
	order := []string{"after-input-1", "after-reply", "after-input-2"}
	for i, want := range order {
		if !strings.Contains(texts[2+i], want) {
			t.Fatalf("entry %d = %q, want %s", 2+i, texts[2+i], want)
		}
	}
	if joined := strings.Join(texts, "\n"); strings.Contains(joined, "before-") {
		t.Fatalf("composition rendered history the checkpoint omits: %q", texts)
	}

	// Without a checkpoint the same history composes whole, without a notice.
	whole := messageTexts(ComposeContextWithCheckpoint(rc, append([]TurnResponseEntry{assistantTR(2500, "before-reply")}, trs...), HistoryCheckpoint{}).Messages)
	if len(whole) != 6 || slices.ContainsFunc(whole, func(text string) bool { return strings.HasPrefix(text, "system:") }) {
		t.Fatalf("composition without a checkpoint = %q", whole)
	}
	// A checkpoint that omits everything there is and holds nothing leaves
	// nothing to open.
	if composed := ComposeContextWithCheckpoint(rc[:2], nil, HistoryCheckpoint{Omitted: held.Omitted, Messages: held.Messages}); composed != nil {
		t.Fatalf("composition of omitted history = %q, want none", messageTexts(composed.Messages))
	}
}

// Under a tight budget the notice stays with what the checkpoint holds while
// older history drops.
func TestComposeBudgetedKeepsTheHistoryNotice(t *testing.T) {
	held := HistoryCheckpoint{Omitted: omittedThrough(1000, 2500, 3), Messages: map[string]CheckpointMessage{"m3": {Kept: true}, "m4": {Kept: true}}}
	rc := RenderedContext{
		textSegment("m3", 3000, strings.Repeat("a", 4000)),
		textSegment("m4", 4000, "newest input"),
	}
	composed, admission := ComposeContextWithCheckpointBudgeted(rc, nil, held, ComposeBudget{MaxTokens: 200})
	if composed == nil || admission.DroppedEntries != 1 || admission.ProtectedOverflow {
		t.Fatalf("composed=%v admission=%+v, want the older input dropped", composed, admission)
	}
	texts := messageTexts(composed.Messages)
	if len(texts) != 2 || texts[0] != "system:"+HistoryNotice(held.Omitted) || !strings.Contains(texts[1], "newest input") {
		t.Fatalf("composed = %q, want the notice and the newest input", texts)
	}
	if !composed.Messages[0].Pinned || composed.Messages[1].Pinned {
		t.Fatalf("only the notice is pinned: %+v", composed.Messages)
	}
}
