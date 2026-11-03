package timeline

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestPipelineIncrementalRenderMatchesFullRender(t *testing.T) {
	t.Parallel()
	params := RenderParams{BotUserID: "bot"}
	pipeline := NewPipelineWithOptions(params, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
	events := []CanonicalEvent{
		MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1, TimestampSec: 1, Sender: &CanonicalUser{ID: "u1", DisplayName: "Alice"}, Content: []ContentNode{{Type: "text", Text: "one"}}},
		MessageEvent{SessionID: "s", MessageID: "m2", ReceivedAtMs: 2, TimestampSec: 2, Sender: &CanonicalUser{ID: "u1", DisplayName: "Alicia"}, Content: []ContentNode{{Type: "text", Text: "two"}}, ReplyToMessageID: "m1"},
		EditEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 3, TimestampSec: 3, Content: []ContentNode{{Type: "text", Text: "edited"}}},
		DeleteEvent{SessionID: "s", MessageIDs: []string{"m2"}, ReceivedAtMs: 4, TimestampSec: 4},
		ServiceEvent{SessionID: "s", Action: ServiceChatRenamed, ReceivedAtMs: 5, TimestampSec: 5, NewTitle: "room"},
		MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 6, TimestampSec: 6, IsSelfSent: true, MentionsMe: true},
	}

	oracle := NewEmptyIC("s")
	for _, event := range events {
		oracle = Reduce(oracle, event)
		got := pipeline.PushEvent("bot", "s", event)
		want := Render(oracle, params)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("incremental render diverged after %#v\ngot:  %#v\nwant: %#v", event, got, want)
		}
	}
}

func TestPipelineRenderedSnapshotsStayImmutable(t *testing.T) {
	t.Parallel()
	pipeline := NewPipelineWithOptions(RenderParams{}, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
	first := pipeline.PushEvent("bot", "s", MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1, Content: []ContentNode{{Type: "text", Text: "before"}}})
	want := first[0].Content[0].Text
	pipeline.PushEvent("bot", "s", EditEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 2, Content: []ContentNode{{Type: "text", Text: "after"}}})
	if first[0].Content[0].Text != want {
		t.Fatalf("previous snapshot mutated: got %q want %q", first[0].Content[0].Text, want)
	}
}

func TestPipelineEvictsByLRUAndTTL(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	pipeline := NewPipelineWithOptions(RenderParams{}, PipelineOptions{
		MaxSessions: 2, MaxResidentByte: -1, TTL: time.Minute, Now: func() time.Time { return now },
	})
	message := func(session string) MessageEvent {
		return MessageEvent{SessionID: session, MessageID: session, Content: []ContentNode{{Type: "text", Text: session}}}
	}
	pipeline.PushEvent("bot", "a", message("a"))
	pipeline.PushEvent("bot", "b", message("b"))
	pipeline.GetRC("a")
	pipeline.PushEvent("bot", "c", message("c"))
	if pipeline.HasSession("b") {
		t.Fatal("least-recently-used session was not evicted")
	}
	now = now.Add(2 * time.Minute)
	if pipeline.HasSession("a") || pipeline.HasSession("c") {
		t.Fatal("expired sessions were not evicted")
	}
	stats := pipeline.Stats()
	if stats.Evictions != 3 || stats.ResidentSessions != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestPipelineEvictsByResidentBytes(t *testing.T) {
	t.Parallel()
	pipeline := NewPipelineWithOptions(RenderParams{}, PipelineOptions{MaxSessions: -1, MaxResidentByte: 1, TTL: -1})
	pipeline.PushEvent("bot", "oversized", MessageEvent{SessionID: "oversized", MessageID: "m", Content: []ContentNode{{Type: "text", Text: "payload"}}})
	if pipeline.HasSession("oversized") {
		t.Fatal("resident-byte limit did not evict oversized session")
	}
}

// Resetting a bot's history drops the sessions of that bot, whether they came
// in by an event or by a replay, and leaves every other bot's sessions warm.
func TestPipelineDropBotDropsOnlyThatBotsSessions(t *testing.T) {
	t.Parallel()
	pipeline := NewPipelineWithOptions(RenderParams{}, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
	pipeline.PushEvent("bot-a", "a1", MessageEvent{SessionID: "a1", MessageID: "m"})
	replay(pipeline, "bot-a", "a2", []CanonicalEvent{MessageEvent{SessionID: "a2", MessageID: "m"}})
	pipeline.PushEvent("bot-b", "b1", MessageEvent{SessionID: "b1", MessageID: "m"})
	replay(pipeline, "bot-b", "b2", []CanonicalEvent{MessageEvent{SessionID: "b2", MessageID: "m"}})

	pipeline.DropBot("bot-a")
	got := pipeline.SessionIDs()
	slices.Sort(got)
	if !slices.Equal(got, []string{"b1", "b2"}) || pipeline.Stats().Evictions != 2 {
		t.Fatalf("resident after dropping bot-a = %v (stats %+v), want b1 and b2", got, pipeline.Stats())
	}
}

// A replay rebuilds its session outside the pipeline's lock while other
// sessions are read and written; run with -race.
func TestPipelineReplaysAlongsideOtherSessions(t *testing.T) {
	t.Parallel()
	pipeline := NewPipelineWithOptions(RenderParams{BotUserID: "bot"}, PipelineOptions{MaxSessions: 4, MaxResidentByte: -1, TTL: -1})
	history := make([]CanonicalEvent, 200)
	for i := range history {
		history[i] = MessageEvent{SessionID: "cold", MessageID: fmt.Sprintf("m%d", i), ReceivedAtMs: int64(i), Content: []ContentNode{{Type: "text", Text: "history"}}}
	}
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("s%d", worker)
			for i := range 50 {
				if worker%2 == 0 {
					replay(pipeline, "bot", session, history)
				} else {
					pipeline.PushEvent("bot", session, MessageEvent{SessionID: session, MessageID: fmt.Sprintf("m%d", i), ReceivedAtMs: int64(i)})
				}
				pipeline.GetRC(session)
				pipeline.HasSession("s0")
			}
		}()
	}
	wg.Wait()
	ic := NewEmptyIC("s0")
	for _, event := range history {
		ic = Reduce(ic, event)
	}
	want := Render(ic, RenderParams{BotUserID: "bot"})
	if got := replay(pipeline, "bot", "s0", history); !reflect.DeepEqual(got, want) {
		t.Fatal("a replay alongside other sessions rendered a different context")
	}
}

func FuzzPipelineIncrementalRenderEquivalence(f *testing.F) {
	f.Add([]byte{0, 0, 1, 2, 3, 4, 1, 0})
	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) > 128 {
			operations = operations[:128]
		}
		pipeline := NewPipelineWithOptions(RenderParams{}, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
		oracle := NewEmptyIC("s")
		for i, operation := range operations {
			messageID := "m1"
			if operation&1 != 0 {
				messageID = "m2"
			}
			var event CanonicalEvent
			switch operation % 5 {
			case 0:
				event = MessageEvent{SessionID: "s", MessageID: messageID, ReceivedAtMs: int64(i), TimestampSec: int64(i), Content: []ContentNode{{Type: "text", Text: string(rune('a' + operation%26))}}}
			case 1:
				event = EditEvent{SessionID: "s", MessageID: messageID, ReceivedAtMs: int64(i), TimestampSec: int64(i), Content: []ContentNode{{Type: "text", Text: string(rune('A' + operation%26))}}}
			case 2:
				event = DeleteEvent{SessionID: "s", MessageIDs: []string{messageID}, ReceivedAtMs: int64(i), TimestampSec: int64(i)}
			case 3:
				event = ServiceEvent{SessionID: "s", Action: ServiceChatRenamed, ReceivedAtMs: int64(i), TimestampSec: int64(i), NewTitle: messageID}
			default:
				event = ServiceEvent{SessionID: "s", Action: ServiceMessagePinned, ReceivedAtMs: int64(i), TimestampSec: int64(i), PinnedMessageID: messageID}
			}
			oracle = Reduce(oracle, event)
			if got, want := pipeline.PushEvent("bot", "s", event), Render(oracle, RenderParams{}); !reflect.DeepEqual(got, want) {
				t.Fatalf("operation %d diverged", i)
			}
		}
	})
}

// A session that is not resident renders the event without becoming resident,
// so its next access still loads its history; a resident one takes the event
// exactly as PushEvent would.
func TestPipelinePushResidentEventNeverMakesASessionResident(t *testing.T) {
	t.Parallel()
	params := RenderParams{BotUserID: "bot"}
	pipeline := NewPipelineWithOptions(params, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
	first := MessageEvent{SessionID: "s", MessageID: "m1", ReceivedAtMs: 1, TimestampSec: 1, Content: []ContentNode{{Type: "text", Text: "one"}}}
	second := MessageEvent{SessionID: "s", MessageID: "m2", ReceivedAtMs: 2, TimestampSec: 2, Content: []ContentNode{{Type: "text", Text: "two"}}}

	alone := pipeline.PushResidentEvent("s", first)
	if want := Render(Reduce(NewEmptyIC("s"), first), params); !reflect.DeepEqual(alone, want) || pipeline.HasSession("s") {
		t.Fatalf("cold session: rendered %#v, resident %v; want %#v, not resident", alone, pipeline.HasSession("s"), want)
	}

	replay(pipeline, "bot", "s", []CanonicalEvent{first})
	got := pipeline.PushResidentEvent("s", second)
	want := Render(Reduce(Reduce(NewEmptyIC("s"), first), second), params)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(pipeline.GetRC("s"), want) {
		t.Fatalf("resident session: rendered %#v, stored %#v; want %#v", got, pipeline.GetRC("s"), want)
	}
}

// replay rebuilds a session of botID from events, as a cold load does.
func replay(pipeline *Pipeline, botID, sessionID string, events []CanonicalEvent) RenderedContext {
	r := pipeline.BeginReplay(botID, sessionID)
	defer r.Close()
	rc, _ := r.Install(events)
	return rc
}

// A drop of a session, or of its bot, after its replay began means the replay
// read history that is gone: it does not install. A drop of another bot does
// not hold it back, and once no replay is in flight the drops are forgotten.
func TestPipelineReplayDoesNotUndoADropThatOvertookIt(t *testing.T) {
	t.Parallel()
	events := []CanonicalEvent{MessageEvent{SessionID: "s", MessageID: "m"}}
	for _, tc := range []struct {
		name    string
		drop    func(*Pipeline)
		install bool
	}{
		{name: "session dropped", drop: func(p *Pipeline) { p.DropSession("s") }},
		{name: "bot dropped", drop: func(p *Pipeline) { p.DropBot("bot-a") }},
		{name: "other bot dropped", drop: func(p *Pipeline) { p.DropBot("bot-b") }, install: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pipeline := NewPipelineWithOptions(RenderParams{}, PipelineOptions{MaxSessions: -1, MaxResidentByte: -1, TTL: -1})
			r := pipeline.BeginReplay("bot-a", "s")
			tc.drop(pipeline)
			_, installed := r.Install(events)
			r.Close()
			if installed != tc.install || pipeline.HasSession("s") != tc.install {
				t.Fatalf("installed = %v, resident = %v; want %v", installed, pipeline.HasSession("s"), tc.install)
			}
			if len(pipeline.droppedSession)+len(pipeline.droppedBot) != 0 {
				t.Fatalf("drops still kept with no replay in flight: %v %v", pipeline.droppedSession, pipeline.droppedBot)
			}
			if replay(pipeline, "bot-a", "s", events) == nil || !pipeline.HasSession("s") {
				t.Fatal("a replay begun after the drop did not install")
			}
		})
	}
}
