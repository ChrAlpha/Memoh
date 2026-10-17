package application

import (
	"testing"

	sdk "github.com/felinics/twilight/sdk"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

func TestMarkRequiredHistoryMessageCurrentUsesExactPersistedID(t *testing.T) {
	t.Parallel()

	cfg := native.RunConfig{
		Messages: []sdk.Message{
			sdk.UserMessage("retry request"),
			sdk.AssistantMessage("old answer"),
			sdk.UserMessage("newer unrelated user"),
		},
		ForkContextSourceMessageIDs: []string{"retry-user", "old-answer", "newer-user"},
	}
	markRequiredHistoryMessageCurrent(&cfg, "retry-user")
	if cfg.ContextCurrentUserMessageIndex == nil || *cfg.ContextCurrentUserMessageIndex != 0 {
		t.Fatalf("current index = %#v, want exact persisted request at 0", cfg.ContextCurrentUserMessageIndex)
	}
}

func TestMarkRequiredHistoryMessageCurrentRejectsNonUserAndMissingIDs(t *testing.T) {
	t.Parallel()

	stale := 2
	cfg := native.RunConfig{
		Messages:                       []sdk.Message{sdk.AssistantMessage("not a request"), sdk.UserMessage("actual user")},
		ForkContextSourceMessageIDs:    []string{"required", "other"},
		ContextCurrentUserMessageIndex: &stale,
	}
	markRequiredHistoryMessageCurrent(&cfg, "required")
	if cfg.ContextCurrentUserMessageIndex != nil {
		t.Fatalf("non-user source ID marked current: %#v", cfg.ContextCurrentUserMessageIndex)
	}
	markRequiredHistoryMessageCurrent(&cfg, "missing")
	if cfg.ContextCurrentUserMessageIndex != nil {
		t.Fatalf("missing source ID marked current: %#v", cfg.ContextCurrentUserMessageIndex)
	}
}
