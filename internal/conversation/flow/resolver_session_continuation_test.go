package flow

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/contextfrag"
	"github.com/memohai/memoh/internal/conversation"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/settings"
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

	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, messages, 0, false, false)

	if len(cfg.ContextHistoryTokenEstimates) != sanitizedCount {
		t.Errorf("ContextHistoryTokenEstimates length = %d, want %d (post-sanitize count, not raw %d)", len(cfg.ContextHistoryTokenEstimates), sanitizedCount, len(messages))
	}
	if cfg.ContextTrimmableMessages != sanitizedCount {
		t.Errorf("ContextTrimmableMessages = %d, want %d (post-sanitize count, not raw %d)", cfg.ContextTrimmableMessages, sanitizedCount, len(messages))
	}
}

func TestFinalizeContinuationRunConfigDefaultsToolExchangePolicy(t *testing.T) {
	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, nil, 0, false, false)

	if cfg.ContextToolExchangePolicy == nil {
		t.Fatal("ContextToolExchangePolicy = nil, want default policy")
	}
	if cfg.ContextToolExchangePolicy.MinMessages != 10 {
		t.Errorf("ContextToolExchangePolicy.MinMessages = %d, want 10", cfg.ContextToolExchangePolicy.MinMessages)
	}
}

func TestFinalizeContinuationRunConfigPreservesExistingToolExchangePolicy(t *testing.T) {
	existing := &contextfrag.ToolExchangePolicy{MinMessages: 3}

	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{ContextToolExchangePolicy: existing}, nil, 0, false, false)

	if cfg.ContextToolExchangePolicy != existing {
		t.Errorf("ContextToolExchangePolicy = %+v, want untouched existing pointer %+v", cfg.ContextToolExchangePolicy, existing)
	}
}

func TestFinalizeContinuationRunConfigDefaultsContextBudgetMaxTokens(t *testing.T) {
	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, nil, 128000, false, false)

	if cfg.ContextBudgetMaxTokens != 128000 {
		t.Errorf("ContextBudgetMaxTokens = %d, want 128000 (default from parameter)", cfg.ContextBudgetMaxTokens)
	}
}

func TestFinalizeContinuationRunConfigPreservesExistingContextBudgetMaxTokens(t *testing.T) {
	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{ContextBudgetMaxTokens: 50000}, nil, 99999, false, false)

	if cfg.ContextBudgetMaxTokens != 50000 {
		t.Errorf("ContextBudgetMaxTokens = %d, want untouched existing value 50000", cfg.ContextBudgetMaxTokens)
	}
}

func TestFinalizeContinuationRunConfigClearsQueryAndSetsStreamFlags(t *testing.T) {
	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{Query: "stale query"}, nil, 0, true, false)
	if cfg.Query != "" {
		t.Errorf("Query = %q, want empty", cfg.Query)
	}
	if !cfg.LiveToolStream {
		t.Error("LiveToolStream = false, want true")
	}
	if cfg.CanRequestUserInput {
		t.Error("CanRequestUserInput = true, want false")
	}

	cfg2 := finalizeContinuationRunConfig(agentpkg.RunConfig{}, nil, 0, false, true)
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

	cfg := finalizeContinuationRunConfig(agentpkg.RunConfig{}, messages, 0, false, false)

	if len(cfg.Messages) != cfg.ContextTrimmableMessages {
		t.Errorf("len(cfg.Messages) = %d, ContextTrimmableMessages = %d; want equal so nothing is appended beyond sanitize(messages)", len(cfg.Messages), cfg.ContextTrimmableMessages)
	}
}

// resumeContextBudgetApplier records the ContextBudgetMaxTokens the agent
// actually received and swaps in a network-free fake model, so
// resumeAgentSession can be driven end-to-end through agent.Stream without
// touching a real provider.
type resumeContextBudgetApplier struct {
	mu       sync.Mutex
	captured int
	provider sdk.Provider
}

func (a *resumeContextBudgetApplier) apply(_ context.Context, cfg agentpkg.RunConfig) agentpkg.RunConfig {
	a.mu.Lock()
	a.captured = cfg.ContextBudgetMaxTokens
	a.mu.Unlock()
	cfg.Model = &sdk.Model{ID: "resume-context-budget-model", Provider: a.provider, Type: sdk.ModelTypeChat}
	return cfg.RefreshContextFrag()
}

func (a *resumeContextBudgetApplier) snapshot() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.captured
}

func TestResumeAgentSessionPropagatesContextBudgetToAgentStream(t *testing.T) {
	t.Parallel()

	conn, queries := newModelSelectionTestDB(t)
	const modelID = "00000000-0000-0000-0000-000000000501"
	const providerID = "00000000-0000-0000-0000-000000000502"
	insertModelSelectionProvider(t, conn, providerID, "openai-completions", true)
	insertModelSelectionModel(t, conn, modelID, "gpt-resume-context-window", providerID, models.ModelTypeChat, true, `{"context_window": 128000}`)

	applier := &resumeContextBudgetApplier{provider: &triggerCaptureProvider{}}
	resolver := &Resolver{
		agent:           agentpkg.New(agentpkg.Deps{ContextViewApplier: applier.apply}),
		modelsService:   models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:         queries,
		settingsService: settings.NewService(slog.New(slog.DiscardHandler), &acpContextBudgetSettingsQueries{chatModelID: modelID}, nil, nil),
		messageService:  &recordingMessageService{},
		logger:          slog.New(slog.DiscardHandler),
	}

	err := resolver.resumeAgentSession(context.Background(), continuationParams{
		BotID:     storeRoundBotID,
		SessionID: "session-1",
	}, nil)
	if err != nil {
		t.Fatalf("resumeAgentSession() error = %v", err)
	}

	if got := applier.snapshot(); got != 128000 {
		t.Fatalf("ContextBudgetMaxTokens seen by agent.Stream = %d, want 128000", got)
	}
}
