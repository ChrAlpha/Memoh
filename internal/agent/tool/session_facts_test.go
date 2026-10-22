package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/mcp"
	"github.com/felinics/memoh/internal/sessionfacts"
)

// The session facts decide, not the mode label the session happens to carry.
func TestToolsReadSessionFactsNotModeLabel(t *testing.T) {
	t.Parallel()

	explicitChat := SessionContext{
		BotID:           "bot_1",
		SessionType:     sessionmode.Chat,
		ReplyDelivery:   sessionfacts.ReplyExplicit,
		CurrentPlatform: "telegram",
		ReplyTarget:     "chat-1",
		LiveStream:      true,
		Emitter:         func(ToolStreamEvent) {},
	}
	if explicitChat.CanUseLocalMessagingShortcut() {
		t.Fatal("explicit delivery must not hand sends to the live stream")
	}
	if desc, _, _, _ := sendToolPromptMetadata(explicitChat); !strings.Contains(desc, "observed conversation") {
		t.Fatalf("explicit delivery send description = %q", desc)
	}
	// A send is the only way an explicit session speaks, so it needs no target.
	sender := &recordingSender{}
	provider := NewMessageProvider(nil, sender, usageTestReactor{}, usageTestResolver{}, messageTestAssetResolver{})
	result, err := provider.execSend(context.Background(), explicitChat, "call-test", map[string]any{"text": "spoken"})
	if err != nil {
		t.Fatalf("execSend returned error: %v", err)
	}
	if resp, _ := result.(map[string]any); sender.called != 1 || resp["delivered"] != "current_conversation" {
		t.Fatalf("explicit delivery send = %#v with %d sends, want one direct send", result, sender.called)
	}

	streamDiscuss := SessionContext{
		SessionType:     sessionmode.Discuss,
		ReplyDelivery:   sessionfacts.ReplyStream,
		CurrentPlatform: "telegram",
		ReplyTarget:     "chat-1",
	}
	if desc, _, _, _ := sendToolPromptMetadata(streamDiscuss); strings.Contains(desc, "observed conversation") {
		t.Fatalf("stream delivery send description = %q", desc)
	}

	if (SessionContext{SessionType: sessionmode.Chat, CanRequestUserInput: true, DecisionMode: sessionfacts.DecisionAutoReject}).CanAskUser() {
		t.Fatal("auto_reject must not offer ask_user")
	}
	if !(SessionContext{SessionType: sessionmode.Discuss, CanRequestUserInput: true, DecisionMode: sessionfacts.DecisionPark}).CanAskUser() {
		t.Fatal("park must offer ask_user")
	}
}

func TestMCPSessionRunsUnderItsModePreset(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"", sessionmode.Chat, sessionmode.ACPAgent, sessionmode.Discuss, sessionmode.Schedule, sessionmode.Subagent} {
		got := sessionFromMCP(mcp.ToolSessionContext{BotID: "bot-1", SessionType: mode})
		want := sessionfacts.Preset(mode)
		if got.ReplyDelivery != want.ReplyDelivery || got.DecisionMode != want.DecisionMode {
			t.Fatalf("%q: facts = %q/%q, want %q/%q", mode, got.ReplyDelivery, got.DecisionMode, want.ReplyDelivery, want.DecisionMode)
		}
	}
}
