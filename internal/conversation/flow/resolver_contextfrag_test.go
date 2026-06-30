package flow

import (
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
)

func TestApplyRunConfigContextAuditPreservesLegacyFieldsAndTurnScope(t *testing.T) {
	t.Parallel()

	cfg := testRunConfigForContextAudit()
	run := TurnRun{
		PersistTurnID: "persist-turn",
		Context:       TurnContextScope{Kind: ContextScopeTurnHead, TurnID: "parent-turn"},
		Variant:       VariantTransition{BaseHeadTurnID: "base-head"},
	}
	req := conversation.ChatRequest{
		BotID:                     "bot-1",
		ChatID:                    "chat-1",
		SessionID:                 "session-1",
		SourceChannelIdentityID:   "sender-1",
		DisplayName:               "Alice",
		CurrentChannel:            "telegram",
		ConversationType:          "group",
		ConversationName:          "Ops",
		ReplyTarget:               "thread-1",
		ExternalMessageID:         "message-1",
		EventID:                   "event-1",
		SourceReplyToMessageID:    "reply-1",
		ForwardMessageID:          "forward-1",
		ForwardFromUserID:         "forward-user",
		ForwardFromConversationID: "forward-chat",
	}

	got := applyRunConfigContextAudit(cfg, req, run, "Resolved Alice", false)

	if got.System != cfg.System || got.Query != cfg.Query || len(got.Messages) != 1 || len(got.InlineImages) != 1 {
		t.Fatalf("legacy fields changed: system=%q query=%q messages=%d images=%d", got.System, got.Query, len(got.Messages), len(got.InlineImages))
	}
	if len(got.ContextFrags) == 0 {
		t.Fatal("context audit should populate frags")
	}
	if got.ContextManifest.View != contextfrag.ViewRunConfigPreProvider {
		t.Fatalf("manifest view = %q", got.ContextManifest.View)
	}
	for _, item := range got.ContextManifest.Items {
		if item.Scope.TurnID != "persist-turn" {
			t.Fatalf("scope lost persist turn id: %#v", item.Scope)
		}
		if item.Scope.ViewHeadTurnID != "parent-turn" {
			t.Fatalf("scope lost view head turn id: %#v", item.Scope)
		}
		if item.Scope.CurrentMessageID != "message-1" || item.Scope.EventID != "event-1" || item.Scope.DisplayName != "Resolved Alice" {
			t.Fatalf("scope lost request metadata: %#v", item.Scope)
		}
	}
}

func TestApplyRunConfigContextAuditOmitsPipelineCurrentQuery(t *testing.T) {
	t.Parallel()

	cfg := testRunConfigForContextAudit()
	got := applyRunConfigContextAudit(cfg, conversation.ChatRequest{BotID: "bot-1", ChatID: "chat-1"}, TurnRun{}, "", true)

	if got.Query != cfg.Query {
		t.Fatalf("legacy query changed: got %q want %q", got.Query, cfg.Query)
	}
	for _, item := range got.ContextManifest.Items {
		if item.Kind == contextfrag.KindCurrentUserMessage {
			t.Fatalf("pipeline audit should not duplicate materialized current query: %#v", item)
		}
	}
}

func testRunConfigForContextAudit() agentpkg.RunConfig {
	return agentpkg.RunConfig{
		System:       "system prompt",
		Messages:     []sdk.Message{sdk.UserMessage("history")},
		Query:        "current query",
		InlineImages: []sdk.ImagePart{{Image: "data:image/png;base64,abc", MediaType: "image/png"}},
	}
}
