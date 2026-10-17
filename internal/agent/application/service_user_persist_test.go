package application

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/felinics/memoh/internal/agent/agenttest"
	"github.com/felinics/memoh/internal/agent/runreq"
)

func TestPersistUserTurnSkillActivationWithoutPromptDoesNotStoreModelMarker(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}
	req := runreq.ChatRequest{
		BotID:           "bot-1",
		ThreadID:        "session-1",
		ModelQuery:      "The user activated the following skill for this turn without an additional prompt: alpha.",
		RawQuery:        "",
		UserMessageKind: runreq.UserMessageKindSkillActivation,
		SkillActivation: &runreq.SkillActivation{
			Skills: []runreq.SkillActivationSkill{{Name: "alpha", DisplayName: "Alpha", State: "effective"}},
		},
	}

	if _, err := resolver.persistUserTurn(context.Background(), req); err != nil {
		t.Fatalf("persistUserTurn() error = %v", err)
	}
	if len(messages.Persisted) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(messages.Persisted))
	}
	got := persistedTextContent(t, messages.Persisted[0].Content)
	if got != "" {
		t.Fatalf("persisted content = %q, want empty prompt only", got)
	}
	if messages.Persisted[0].DisplayText != "" {
		t.Fatalf("display text = %q, want empty", messages.Persisted[0].DisplayText)
	}
	if messages.Persisted[0].Metadata["user_message_kind"] != runreq.UserMessageKindSkillActivation {
		t.Fatalf("metadata kind = %#v, want skill_activation", messages.Persisted[0].Metadata["user_message_kind"])
	}
}

func TestPersistUserTurnSkillActivationWithPromptStoresPromptOnly(t *testing.T) {
	t.Parallel()

	messages := &agenttest.RecordingMessages{}
	resolver := &Service{
		messageService: messages,
		logger:         slog.New(slog.DiscardHandler),
	}
	req := runreq.ChatRequest{
		BotID:           "bot-1",
		ThreadID:        "session-1",
		Query:           "Plan the widget implementation",
		RawQuery:        "Plan the widget implementation",
		UserVisibleText: "Plan the widget implementation",
		UserMessageKind: runreq.UserMessageKindSkillActivation,
		SkillActivation: &runreq.SkillActivation{
			Prompt: "Plan the widget implementation",
			Skills: []runreq.SkillActivationSkill{{Name: "alpha", DisplayName: "Alpha", State: "effective"}},
		},
	}

	if _, err := resolver.persistUserTurn(context.Background(), req); err != nil {
		t.Fatalf("persistUserTurn() error = %v", err)
	}
	if len(messages.Persisted) != 1 {
		t.Fatalf("persisted messages = %d, want 1", len(messages.Persisted))
	}
	got := persistedTextContent(t, messages.Persisted[0].Content)
	if got != "Plan the widget implementation" {
		t.Fatalf("persisted content = %q, want prompt only", got)
	}
	if messages.Persisted[0].DisplayText != "Plan the widget implementation" {
		t.Fatalf("display text = %q, want prompt", messages.Persisted[0].DisplayText)
	}
}

func persistedTextContent(t *testing.T, content json.RawMessage) string {
	t.Helper()
	var msg runreq.ModelMessage
	if err := json.Unmarshal(content, &msg); err != nil {
		t.Fatalf("unmarshal persisted content: %v", err)
	}
	return msg.TextContent()
}
