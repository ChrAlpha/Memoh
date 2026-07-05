package flow

import (
	"testing"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
)

func TestFinalizeContinuationRunConfigSetsHistorySignalsFromSanitizedMessages(t *testing.T) {
	messages := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{Role: "", Content: conversation.NewTextContent("dropped")},
		{Role: "assistant", Content: conversation.NewTextContent("hi there")},
	}

	sanitizedCount := len(sanitizeMessages(messages))
	if sanitizedCount != 2 {
		t.Fatalf("test fixture is broken: expected sanitizeMessages to drop the empty-role message, got %d messages", sanitizedCount)
	}

	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, messages, false, false)

	if len(cfg.ContextHistoryTokenEstimates) != sanitizedCount {
		t.Errorf("ContextHistoryTokenEstimates length = %d, want %d (post-sanitize count, not raw %d)", len(cfg.ContextHistoryTokenEstimates), sanitizedCount, len(messages))
	}
	if cfg.ContextTrimmableMessages != sanitizedCount {
		t.Errorf("ContextTrimmableMessages = %d, want %d (post-sanitize count, not raw %d)", cfg.ContextTrimmableMessages, sanitizedCount, len(messages))
	}
}

func TestFinalizeContinuationRunConfigDefaultsToolExchangePolicy(t *testing.T) {
	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, nil, false, false)

	if cfg.ContextToolExchangePolicy == nil {
		t.Fatal("ContextToolExchangePolicy = nil, want default policy")
	}
	if cfg.ContextToolExchangePolicy.MinMessages != 10 {
		t.Errorf("ContextToolExchangePolicy.MinMessages = %d, want 10", cfg.ContextToolExchangePolicy.MinMessages)
	}
}

func TestFinalizeContinuationRunConfigPreservesExistingToolExchangePolicy(t *testing.T) {
	existing := &contextfrag.ToolExchangePolicy{MinMessages: 3}

	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{ContextToolExchangePolicy: existing}, nil, false, false)

	if cfg.ContextToolExchangePolicy != existing {
		t.Errorf("ContextToolExchangePolicy = %+v, want untouched existing pointer %+v", cfg.ContextToolExchangePolicy, existing)
	}
}

func TestFinalizeContinuationRunConfigClearsQueryAndSetsStreamFlags(t *testing.T) {
	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{Query: "stale query"}, nil, true, false)
	if cfg.Query != "" {
		t.Errorf("Query = %q, want empty", cfg.Query)
	}
	if !cfg.LiveToolStream {
		t.Error("LiveToolStream = false, want true")
	}
	if cfg.CanRequestUserInput {
		t.Error("CanRequestUserInput = true, want false")
	}

	cfg2 := finalizeContinuationRunConfig(agentpkg.RunConfig{}, nil, false, true)
	if cfg2.LiveToolStream {
		t.Error("LiveToolStream = true, want false")
	}
	if !cfg2.CanRequestUserInput {
		t.Error("CanRequestUserInput = false, want true")
	}
}

func TestFinalizeContinuationRunConfigMessagesMatchTrimmableCount(t *testing.T) {
	messages := []conversation.ModelMessage{
		{Role: "user", Content: conversation.NewTextContent("hello")},
		{Role: "assistant", Content: conversation.NewTextContent("hi there")},
	}

	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, messages, false, false)

	if len(cfg.Messages) != cfg.ContextTrimmableMessages {
		t.Errorf("len(cfg.Messages) = %d, ContextTrimmableMessages = %d; want equal so nothing is appended beyond sanitize(messages)", len(cfg.Messages), cfg.ContextTrimmableMessages)
	}
}
