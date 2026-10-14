package local

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/channel"
)

// subscribe returns what a subscriber of the route sees, in order.
func subscribe(t *testing.T, hub *RouteHub, route string) func() []channel.StreamEvent {
	t.Helper()
	_, events, cancel := hub.Subscribe(route)
	t.Cleanup(cancel)
	return func() []channel.StreamEvent {
		var got []channel.StreamEvent
		for {
			select {
			case item := <-events:
				got = append(got, item.Event)
			case <-time.After(20 * time.Millisecond):
				return got
			}
		}
	}
}

func types(events []channel.StreamEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, string(ev.Type)+"/"+string(ev.Status))
	}
	return out
}

func openTestPreview(t *testing.T) (channel.PreparedPreview, func() []channel.StreamEvent) {
	t.Helper()
	hub := NewRouteHub()
	seen := subscribe(t, hub, "bot-1")
	preview, err := NewWebAdapter(hub).OpenPreview(context.Background(), channel.ChannelConfig{}, " bot-1 ", channel.StreamOptions{})
	if err != nil {
		t.Fatalf("OpenPreview: %v", err)
	}
	return preview, seen
}

func finalOf(msg channel.PreparedMessage) *channel.PreparedStreamEvent {
	return &channel.PreparedStreamEvent{Type: channel.StreamEventFinal, Final: &channel.PreparedStreamFinalizePayload{Message: msg}}
}

func TestWebAdapterDeclaresANativeStream(t *testing.T) {
	t.Parallel()

	var _ channel.PreviewOpener = (*WebAdapter)(nil)
	caps := NewWebAdapter(nil).Descriptor().Capabilities
	if caps.Preview != channel.PreviewNativeStream {
		t.Fatalf("preview mode = %v, want native stream", caps.Preview)
	}
	if !caps.NativeApproval || !caps.NativeUserInput {
		t.Fatal("the Web UI renders approval requests and questions from the stream itself")
	}
}

func TestWebAdapterOpenPreviewRejectsMissingHubTargetAndCanceledContext(t *testing.T) {
	t.Parallel()

	if _, err := NewWebAdapter(nil).OpenPreview(context.Background(), channel.ChannelConfig{}, "bot-1", channel.StreamOptions{}); err == nil {
		t.Fatal("expected an error without a hub")
	}
	adapter := NewWebAdapter(NewRouteHub())
	if _, err := adapter.OpenPreview(context.Background(), channel.ChannelConfig{}, "  ", channel.StreamOptions{}); err == nil {
		t.Fatal("expected an error without a target")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.OpenPreview(ctx, channel.ChannelConfig{}, "bot-1", channel.StreamOptions{}); err == nil {
		t.Fatal("expected an error for a canceled context")
	}
}

// Every event of the reply reaches the subscribers of the route as it is, an
// error included: the stream has no placeholder to put it in place of.
func TestLocalPreviewForwardsEventsAsTheyHappen(t *testing.T) {
	t.Parallel()

	preview, seen := openTestPreview(t)
	ctx := context.Background()
	events := []channel.PreparedStreamEvent{
		{Type: channel.StreamEventStatus, Status: channel.StreamStatusStarted},
		{Type: channel.StreamEventDelta, Delta: "hello", Phase: channel.StreamPhaseText},
		{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{Name: "read", CallID: "c1"}},
		{Type: channel.StreamEventError, Error: "boom", ErrorCode: "internal"},
	}
	for _, ev := range events {
		if err := preview.Update(ctx, ev); err != nil {
			t.Fatalf("update %s: %v", ev.Type, err)
		}
	}

	got := seen()
	want := []string{"status/started", "delta/", "tool_call_start/", "error/"}
	if strings.Join(types(got), ",") != strings.Join(want, ",") {
		t.Fatalf("subscriber saw %v, want %v", types(got), want)
	}
	if got[1].Delta != "hello" || got[3].Error != "boom" || got[3].ErrorCode != "internal" {
		t.Fatalf("events changed on the way: %+v", got)
	}
}

// The final reaches subscribers with its attachments, and the completed status
// closes the reply.
func TestLocalPreviewFinishPublishesTheFinalThenCompleted(t *testing.T) {
	t.Parallel()

	preview, seen := openTestPreview(t)
	err := preview.Finish(context.Background(), finalOf(channel.PreparedMessage{
		Message: channel.Message{Text: "done"},
		Attachments: []channel.PreparedAttachment{{
			Logical: channel.Attachment{Type: channel.AttachmentFile, Name: "a.txt"},
			Kind:    channel.PreparedAttachmentUpload,
			Name:    "a.txt",
			Open: func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("a")), nil
			},
		}},
	}))
	if err != nil {
		t.Fatalf("finish: %v", err)
	}

	got := seen()
	if strings.Join(types(got), ",") != "final/,status/completed" {
		t.Fatalf("subscriber saw %v", types(got))
	}
	if got[0].Final.Message.Text != "done" || len(got[0].Final.Message.Attachments) != 1 || got[0].Final.Message.Attachments[0].Name != "a.txt" {
		t.Fatalf("final = %+v", got[0].Final)
	}
}

// A run that said nothing still closes the reply for subscribers.
func TestLocalPreviewFinishWithoutFinalPublishesCompleted(t *testing.T) {
	t.Parallel()

	preview, seen := openTestPreview(t)
	if err := preview.Finish(context.Background(), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if got := types(seen()); strings.Join(got, ",") != "status/completed" {
		t.Fatalf("subscriber saw %v", got)
	}
}

func TestLocalPreviewFinishTakesEffectOnce(t *testing.T) {
	t.Parallel()

	preview, seen := openTestPreview(t)
	ctx := context.Background()
	if err := preview.Finish(ctx, finalOf(channel.PreparedMessage{Message: channel.Message{Text: "done"}})); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := preview.Finish(ctx, finalOf(channel.PreparedMessage{Message: channel.Message{Text: "again"}})); err != nil {
		t.Fatalf("second finish: %v", err)
	}
	if err := preview.Finish(ctx, nil); err != nil {
		t.Fatalf("third finish: %v", err)
	}
	if err := preview.Update(ctx, channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: "late"}); err != nil {
		t.Fatalf("late update: %v", err)
	}

	if got := types(seen()); strings.Join(got, ",") != "final/,status/completed" {
		t.Fatalf("subscriber saw %v, want the first finish only", got)
	}
}

// Abort adds no completed status, and nothing is shown after it.
func TestLocalPreviewAbortShowsNothingMore(t *testing.T) {
	t.Parallel()

	preview, seen := openTestPreview(t)
	ctx := context.Background()
	if err := preview.Update(ctx, channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: "partial"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := preview.Abort(ctx); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if err := preview.Update(ctx, channel.PreparedStreamEvent{Type: channel.StreamEventDelta, Delta: "late"}); err != nil {
		t.Fatalf("late update: %v", err)
	}
	if err := preview.Finish(ctx, finalOf(channel.PreparedMessage{Message: channel.Message{Text: "late"}})); err != nil {
		t.Fatalf("late finish: %v", err)
	}

	if got := types(seen()); strings.Join(got, ",") != "delta/" {
		t.Fatalf("subscriber saw %v, want the partial text only", got)
	}
}
