package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/channel"
)

func openTestStream(t *testing.T, surface *surfaceLog, hide bool) channel.OutboundStream {
	t.Helper()
	stream, err := OpenStream(context.Background(), Config{
		Surface: surface, Target: "chat-1", Reply: reply, SourceMessageID: "m-1", HideToolCalls: hide,
	})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	return stream
}

func push(t *testing.T, stream channel.OutboundStream, events ...channel.StreamEvent) {
	t.Helper()
	for _, event := range events {
		if err := stream.Push(context.Background(), event); err != nil {
			t.Fatalf("push %s: %v", event.Type, err)
		}
	}
}

func finalMessage(text string) channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventFinal, Final: &channel.StreamFinalizePayload{Message: channel.Message{Text: text}}}
}

var completed = channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusCompleted}

// A caller that pushes events itself gets the rules of a run's reply: a prompt
// the channel cannot render is a message between two previews.
func TestStreamSplitsThePreviewAroundAPlainPrompt(t *testing.T) {
	surface := &surfaceLog{caps: streaming}
	stream := openTestStream(t, surface, false)
	push(t, stream,
		channel.StreamEvent{Type: channel.StreamEventDelta, Delta: "text", Phase: channel.StreamPhaseText},
		approval(),
	)
	got := surface.snapshot()
	if len(got) != 6 || got[2] != "finish:flush" || !strings.HasPrefix(got[3], "send:") || got[4] != "open" {
		t.Fatalf("calls = %v, want the preview ended, the prompt sent, a new preview opened", got)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// The first message ends the preview and later ones are messages of their own,
// once the stream completes.
func TestStreamEndsWithItsFirstMessage(t *testing.T) {
	surface := &surfaceLog{caps: streaming}
	stream := openTestStream(t, surface, false)
	push(t, stream, finalMessage("first"), finalMessage("second"))
	if got := surface.snapshot(); !equalCalls(got, "open") {
		t.Fatalf("calls before completion = %v", got)
	}
	push(t, stream, completed)
	if got := surface.snapshot(); !equalCalls(got, "open", "finish:first", "send:second") {
		t.Fatalf("calls = %v", got)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := surface.snapshot(); len(got) != 3 {
		t.Fatalf("close after completion changed the reply: %v", got)
	}
}

// A stream closed without completing aborts the preview, and an error before
// any text ends it.
func TestStreamCloseAbortsAnUnfinishedReply(t *testing.T) {
	surface := &surfaceLog{caps: streaming}
	stream := openTestStream(t, surface, false)
	push(t, stream, channel.StreamEvent{Type: channel.StreamEventError, Error: "failed"})
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := surface.snapshot(); !equalCalls(got, "open", "update:error", "abort") {
		t.Fatalf("calls = %v", got)
	}
}

// Hidden tool calls are dropped; a prompt the user answers is not.
func TestStreamHidesToolCallsButNotPrompts(t *testing.T) {
	surface := &surfaceLog{caps: streaming}
	stream := openTestStream(t, surface, true)
	push(t, stream, toolStart(), toolEnd(), askUser())
	got := surface.snapshot()
	if len(got) != 2 || got[0] != "open" || !strings.HasPrefix(got[1], "send:") {
		t.Fatalf("calls = %v, want only the ask_user prompt", got)
	}
}
