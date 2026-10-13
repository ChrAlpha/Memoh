package timeline

import (
	"reflect"
	"testing"
)

func TestBuildRCMatchesReplaySession(t *testing.T) {
	t.Parallel()
	params := RenderParams{BotUserID: "bot"}
	events := []CanonicalEvent{
		MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1, TimestampSec: 1, Sender: &CanonicalUser{ID: "u1", DisplayName: "Alice"}, Content: []ContentNode{{Type: "text", Text: "one"}}},
		MessageEvent{SessionID: "s", MessageID: "m2", ReceivedAtMs: 2, TimestampSec: 2, Sender: &CanonicalUser{ID: "u1", DisplayName: "Alice"}, Content: []ContentNode{{Type: "text", Text: "two"}}, ReplyToMessageID: "m1", ReplyToSender: "bot"},
		EditEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 3, TimestampSec: 3, Content: []ContentNode{{Type: "text", Text: "edited"}}},
		DeleteEvent{SessionID: "s", MessageIDs: []string{"m2"}, ReceivedAtMs: 4, TimestampSec: 4},
		ServiceEvent{SessionID: "s", Action: ServiceChatRenamed, ReceivedAtMs: 5, TimestampSec: 5, NewTitle: "room"},
		MessageEvent{SessionID: "s", MessageID: "m3", ReceivedAtMs: 6, TimestampSec: 6, MentionsMe: true, Content: []ContentNode{{Type: "text", Text: "hi"}}},
	}
	pipeline := NewPipelineWithOptions(params, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
	want := pipeline.ReplaySession("s", events)
	got := BuildRC(events, params)
	if len(got) == 0 {
		t.Fatal("expected a non-empty rendered context")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildRC diverged from ReplaySession\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestBuildRCEmpty(t *testing.T) {
	t.Parallel()
	if got := BuildRC(nil, RenderParams{}); len(got) != 0 {
		t.Fatalf("empty events must render nothing, got %#v", got)
	}
}
