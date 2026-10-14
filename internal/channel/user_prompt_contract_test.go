package channel_test

import (
	"context"
	"testing"
	"time"

	"github.com/felinics/memoh/internal/channel"
	localadapter "github.com/felinics/memoh/internal/channel/adapters/local"
)

func approvalEvent() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "exec", CallID: "call-1", ApprovalID: "approval-1", ShortID: 7,
		Input: map[string]any{"command": "pwd"},
	}}
}

func askUserEvent() channel.StreamEvent {
	return channel.StreamEvent{Type: channel.StreamEventToolCallStart, ToolCall: &channel.StreamToolCall{
		Name: "ask_user", CallID: "call-2",
		Actions: []channel.Action{{Type: "user_input", Value: "respond:input-1"}},
		Input: map[string]any{"payload": map[string]any{"questions": []any{map[string]any{
			"text": "Pick one", "kind": "single_select",
			"options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B"}},
		}}}},
	}}
}

// The Web channel hands both events to the route hub, where the web UI
// renders them.
func TestLocalAdapterForwardsApprovalAndUserInputEvents(t *testing.T) {
	t.Parallel()

	hub := localadapter.NewRouteHub()
	adapter := localadapter.NewWebAdapter(hub)
	reg := channel.NewRegistry()
	reg.MustRegister(adapter)
	manager := channel.NewManager(nil, reg, nil, nil)
	cfg := channel.ChannelConfig{BotID: "bot-1", ChannelType: adapter.Type()}
	_, events, cancel := hub.Subscribe("target-1")
	defer cancel()
	preview, err := channel.NewReplySenderForTest(manager, cfg, adapter.Type()).OpenPreview(context.Background(), "target-1", channel.StreamOptions{})
	if err != nil {
		t.Fatalf("open preview: %v", err)
	}
	for _, event := range []channel.StreamEvent{approvalEvent(), askUserEvent()} {
		if err := preview.Update(context.Background(), event); err != nil {
			t.Fatalf("update: %v", err)
		}
		select {
		case got := <-events:
			if got.Event.Type != channel.StreamEventToolCallStart || got.Event.ToolCall == nil || got.Event.ToolCall.CallID != event.ToolCall.CallID {
				t.Fatalf("hub event = %#v, want %s forwarded", got.Event, event.ToolCall.CallID)
			}
		case <-time.After(time.Second):
			t.Fatalf("hub did not receive %s", event.ToolCall.CallID)
		}
	}
}
