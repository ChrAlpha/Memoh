package application

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"
	sdk "github.com/memohai/twilight-ai/sdk"

	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	"github.com/memohai/memoh/internal/apperror"
	messagepkg "github.com/memohai/memoh/internal/chat/message"
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

type resumeTextProvider struct {
	triggerCaptureProvider
}

func (p *resumeTextProvider) DoStream(ctx context.Context, params sdk.GenerateParams) (*sdk.StreamResult, error) {
	result, err := p.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}
	ch := make(chan sdk.StreamPart, 8)
	go func() {
		defer close(ch)
		ch <- &sdk.StartPart{}
		ch <- &sdk.StartStepPart{}
		ch <- &sdk.TextStartPart{ID: "resume-text"}
		ch <- &sdk.TextDeltaPart{ID: "resume-text", Text: result.Text}
		ch <- &sdk.TextEndPart{ID: "resume-text"}
		ch <- &sdk.FinishStepPart{FinishReason: result.FinishReason}
		ch <- &sdk.FinishPart{FinishReason: result.FinishReason}
	}()
	return &sdk.StreamResult{
		Stream:   ch,
		Messages: []sdk.Message{sdk.AssistantMessage(result.Text)},
	}, nil
}

type failingContinuationMessageService struct {
	*recordingMessageService
	err   error
	calls int
}

func (s *failingContinuationMessageService) Persist(
	_ context.Context,
	_ messagepkg.PersistInput,
) (messagepkg.Message, error) {
	s.calls++
	return messagepkg.Message{}, s.err
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

func TestRuntimeOwnedContinuationPersistsInMemoryLifecycleWithoutAssistantMetadata(t *testing.T) {
	const (
		modelID    = "00000000-0000-0000-0000-000000000511"
		providerID = "00000000-0000-0000-0000-000000000512"
	)
	storeErr := errors.New("assistant message store unavailable")
	provider := modelSelectionProviderRow(t, providerID, "openai-completions", true)
	model := modelSelectionModelRow(t, modelID, "gpt-resume-store-error", provider.ID, models.ModelTypeChat, true)
	model.Config = []byte(`{"context_window": 128000}`)
	queries := &modelSelectionFakeQueries{
		models:   map[string]sqlc.Model{model.ModelID: model},
		provider: provider,
	}
	lifecycles := &recordingContextLifecycleQueries{}
	applier := &resumeContextBudgetApplier{provider: &resumeTextProvider{}}
	resolver := &Service{
		agent: agentpkg.New(agentpkg.Deps{ContextViewApplier: func(ctx context.Context, cfg agentpkg.RunConfig) (agentpkg.RunConfig, error) {
			cfg.Messages = []sdk.Message{sdk.UserMessage("continue")}
			return applier.apply(ctx, cfg)
		}}),
		modelsService:     models.NewService(slog.New(slog.DiscardHandler), queries),
		queries:           queries,
		settingsService:   settings.NewService(slog.New(slog.DiscardHandler), &acpContextBudgetSettingsQueries{chatModelID: modelID}, nil, nil),
		messageService:    &failingContinuationMessageService{recordingMessageService: &recordingMessageService{}, err: storeErr},
		contextLifecycles: lifecycles,
		logger:            slog.New(slog.DiscardHandler),
	}
	runtimeLifecycle := &continuationLifecycleResult{}

	err := resolver.resumeAgentSession(context.Background(), continuationParams{
		BotID:            lifecycleTestBotID,
		SessionID:        lifecycleTestSessionID,
		RunID:            lifecycleTestRunID,
		RuntimeLifecycle: runtimeLifecycle,
	}, nil)
	if err != nil {
		t.Fatalf("resumeAgentSession() error = %v, want nil", err)
	}
	if runtimeLifecycle.snapshot == nil {
		t.Fatal("runtime-owned continuation lost its in-memory lifecycle snapshot")
	}
	if got := resolver.messageService.(*failingContinuationMessageService).calls; got == 0 {
		t.Fatal("test did not exercise assistant message persistence")
	}
	if len(lifecycles.params) != 0 {
		t.Fatalf("inner continuation lifecycle writes = %d, want 0", len(lifecycles.params))
	}

	publicationErr := errors.New("runtime publication failed after assistant store")
	resolver.persistRuntimeDecisionLifecycle(context.Background(), sessionruntime.Command{
		RunID:     lifecycleTestRunID,
		BotID:     lifecycleTestBotID,
		SessionID: lifecycleTestSessionID,
	}, runtimeLifecycle, publicationErr)

	if len(lifecycles.params) != 1 {
		t.Fatalf("runtime lifecycle writes = %d, want 1", len(lifecycles.params))
	}
	if got := lifecycles.params[0].Status; got != contextLifecycleStatusFailedProvider {
		t.Fatalf("runtime lifecycle status = %q, want %q", got, contextLifecycleStatusFailedProvider)
	}
}

func TestPersistRuntimeDecisionLifecycleSkipsExplicitOwnershipLoss(t *testing.T) {
	cfg := lifecycleTestRunConfig(t, lifecycleTestRunID)
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	lifecycles := &recordingContextLifecycleQueries{}
	resolver := &Service{contextLifecycles: lifecycles}

	resolver.persistRuntimeDecisionLifecycle(context.Background(), sessionruntime.Command{
		RunID:     lifecycleTestRunID,
		BotID:     lifecycleTestBotID,
		SessionID: lifecycleTestSessionID,
	}, &continuationLifecycleResult{snapshot: &snapshot}, sessionruntime.ErrRunOwnershipLost)

	if len(lifecycles.params) != 0 {
		t.Fatalf("stale-owner lifecycle writes = %d, want 0", len(lifecycles.params))
	}
}

func TestRuntimeDecisionTerminalDoesNotExposePrivateErrors(t *testing.T) {
	tests := []struct {
		name    string
		cause   error
		status  string
		message string
	}{
		{name: "success"},
		{name: "canceled", cause: context.Canceled},
		{
			name:   "private provider error",
			cause:  errors.New("private provider detail"),
			status: sessionruntime.RunStatusErrored,
		},
		{
			name:    "stable application error",
			cause:   apperror.New(apperror.CodeContextBudgetUnsatisfied, nil),
			status:  sessionruntime.RunStatusErrored,
			message: string(apperror.CodeContextBudgetUnsatisfied),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, message := runtimeDecisionTerminal(tt.cause)
			if status != tt.status || message != tt.message {
				t.Fatalf("runtimeDecisionTerminal() = (%q, %q), want (%q, %q)", status, message, tt.status, tt.message)
			}
		})
	}
}
