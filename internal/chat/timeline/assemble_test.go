package timeline

import (
	"reflect"
	"testing"
)

func TestBuildRCMatchesRenderOfReducedIC(t *testing.T) {
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
	ic := NewEmptyIC("s")
	for _, event := range events {
		reduceInPlace(&ic, event)
	}
	want := Render(ic, params)
	got := BuildRC(events, params)
	if len(got) == 0 {
		t.Fatal("expected a non-empty rendered context")
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildRC diverged from Render over the reduced IC\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestBuildRCEmpty(t *testing.T) {
	t.Parallel()
	if got := BuildRC(nil, RenderParams{}); len(got) != 0 {
		t.Fatalf("empty events must render nothing, got %#v", got)
	}
}

// eventFold renders the events pushed so far, so tests can assert on the
// rendered context after each event without a resident cache.
type eventFold struct{ events []CanonicalEvent }

func (f *eventFold) PushEvent(_ string, event CanonicalEvent) RenderedContext {
	f.events = append(f.events, event)
	return BuildRC(f.events, RenderParams{})
}
