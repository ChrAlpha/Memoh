package flow

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/settings"
)

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

	const modelID = "00000000-0000-0000-0000-000000000501"
	const providerID = "00000000-0000-0000-0000-000000000502"
	provider := modelSelectionProviderRow(t, providerID, "openai-completions", true)
	model := modelSelectionModelRow(t, modelID, "gpt-resume-context-window", provider.ID, models.ModelTypeChat, true)
	model.Config = []byte(`{"context_window": 128000}`)
	queries := &modelSelectionFakeQueries{
		models:   map[string]sqlc.Model{model.ModelID: model},
		provider: provider,
	}

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
		SessionID: "00000000-0000-0000-0000-000000000503",
	}, nil)
	if err != nil {
		t.Fatalf("resumeAgentSession() error = %v", err)
	}

	if got := applier.snapshot(); got != 128000 {
		t.Fatalf("ContextBudgetMaxTokens seen by agent.Stream = %d, want 128000", got)
	}
}
