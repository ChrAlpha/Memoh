package application

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
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
	runID    string
	provider sdk.Provider
}

func (a *resumeContextBudgetApplier) apply(_ context.Context, cfg agentpkg.RunConfig) (agentpkg.RunConfig, error) {
	a.mu.Lock()
	a.captured = cfg.ContextBudgetMaxTokens
	a.runID = cfg.RunID
	a.mu.Unlock()
	cfg.Model = &sdk.Model{ID: "resume-context-budget-model", Provider: a.provider, Type: sdk.ModelTypeChat}
	return cfg.RefreshContextFrag(), nil
}

func (a *resumeContextBudgetApplier) snapshot() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.captured
}

func (a *resumeContextBudgetApplier) snapshotRunID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runID
}

func TestResumeAgentSessionPropagatesContextBudgetToAgentStream(t *testing.T) {
	t.Parallel()

	const modelID = "00000000-0000-0000-0000-000000000501"
	const providerID = "00000000-0000-0000-0000-000000000502"
	const admittedRunID = "00000000-0000-0000-0000-000000000504"
	provider := modelSelectionProviderRow(t, providerID, "openai-completions", true)
	model := modelSelectionModelRow(t, modelID, "gpt-resume-context-window", provider.ID, models.ModelTypeChat, true)
	model.Config = []byte(`{"context_window": 128000}`)
	queries := &modelSelectionFakeQueries{
		models:   map[string]sqlc.Model{model.ModelID: model},
		provider: provider,
	}

	applier := &resumeContextBudgetApplier{provider: &triggerCaptureProvider{}}
	resolver := &Service{
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
		RunID:     admittedRunID,
	}, nil)
	if err != nil {
		t.Fatalf("resumeAgentSession() error = %v", err)
	}

	if got := applier.snapshot(); got != 128000 {
		t.Fatalf("ContextBudgetMaxTokens seen by agent.Stream = %d, want 128000", got)
	}
	if got := applier.snapshotRunID(); got != admittedRunID {
		t.Fatalf("RunID seen by agent.Stream = %q, want admitted run ID %q", got, admittedRunID)
	}

	err = resolver.resumeAgentSession(context.Background(), continuationParams{
		BotID:     storeRoundBotID,
		SessionID: "00000000-0000-0000-0000-000000000503",
	}, nil)
	if err != nil {
		t.Fatalf("legacy resumeAgentSession() error = %v", err)
	}
	legacyRunID := applier.snapshotRunID()
	if _, err := uuid.Parse(legacyRunID); err != nil {
		t.Fatalf("legacy RunID seen by agent.Stream = %q, want minted UUID: %v", legacyRunID, err)
	}
	if legacyRunID == admittedRunID {
		t.Fatalf("legacy continuation reused unrelated admitted run ID %q", legacyRunID)
	}
}
