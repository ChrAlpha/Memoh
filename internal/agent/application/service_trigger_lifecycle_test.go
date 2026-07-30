package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	sdk "github.com/memohai/twilight-ai/sdk"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	agentpkg "github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	"github.com/memohai/memoh/internal/agent/turn"
	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/contextview"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	"github.com/memohai/memoh/internal/heartbeat"
	"github.com/memohai/memoh/internal/models"
	"github.com/memohai/memoh/internal/settings"
)

const (
	triggerLifecycleModelID        = "00000000-0000-4000-8000-000000000901"
	triggerLifecycleProviderID     = "00000000-0000-4000-8000-000000000902"
	triggerLifecyclePromptMarker   = "PRIVATE_TRIGGER_PROMPT_MARKER"
	triggerLifecycleResponseMarker = "PRIVATE_TRIGGER_RESPONSE_MARKER"
)

type triggerLifecycleQueries struct {
	modelSelectionFakeQueries
	modelID     string
	settingsErr error
}

func (q *triggerLifecycleQueries) GetSettingsByBotID(
	_ context.Context,
	botID pgtype.UUID,
) (sqlc.GetSettingsByBotIDRow, error) {
	if q.settingsErr != nil {
		return sqlc.GetSettingsByBotIDRow{}, q.settingsErr
	}
	return sqlc.GetSettingsByBotIDRow{
		BotID:             botID,
		Language:          "auto",
		ReasoningEffort:   "medium",
		HeartbeatInterval: 30,
		CompactionRatio:   80,
		ChatModelID:       flowTestUUID(q.modelID),
	}, nil
}

type triggerLifecycleProvider struct {
	mu            sync.Mutex
	calls         int
	params        sdk.GenerateParams
	nonClosing    bool
	streamOnce    sync.Once
	streamStarted chan struct{}
}

func (*triggerLifecycleProvider) Name() string { return "trigger-lifecycle" }

func (*triggerLifecycleProvider) ListModels(context.Context) ([]sdk.Model, error) { return nil, nil }

func (*triggerLifecycleProvider) Test(context.Context) *sdk.ProviderTestResult {
	return &sdk.ProviderTestResult{Status: sdk.ProviderStatusOK}
}

func (*triggerLifecycleProvider) TestModel(context.Context, string) (*sdk.ModelTestResult, error) {
	return &sdk.ModelTestResult{Supported: true}, nil
}

func (p *triggerLifecycleProvider) DoGenerate(
	_ context.Context,
	params sdk.GenerateParams,
) (*sdk.GenerateResult, error) {
	p.mu.Lock()
	p.calls++
	p.params = params
	p.mu.Unlock()
	return &sdk.GenerateResult{
		Text:         "HEARTBEAT_OK " + triggerLifecycleResponseMarker,
		FinishReason: sdk.FinishReasonStop,
		Messages: []sdk.Message{
			sdk.AssistantMessage("HEARTBEAT_OK " + triggerLifecycleResponseMarker),
		},
	}, nil
}

func (p *triggerLifecycleProvider) DoStream(
	context.Context,
	sdk.GenerateParams,
) (*sdk.StreamResult, error) {
	if p.nonClosing {
		p.streamOnce.Do(func() {
			if p.streamStarted != nil {
				close(p.streamStarted)
			}
		})
		return &sdk.StreamResult{Stream: make(chan sdk.StreamPart)}, nil
	}
	return nil, errors.New("unexpected streaming call")
}

func (p *triggerLifecycleProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

type triggerLifecycleFixture struct {
	service    *Service
	runtime    *lifecycleSubagentRuntime
	lifecycles *recordingContextLifecycleQueries
	messages   *recordingMessageService
	provider   *triggerLifecycleProvider
	queries    *triggerLifecycleQueries
}

func newTriggerLifecycleFixture(
	t *testing.T,
	runID string,
	contextWindow int,
	lifecycles *recordingContextLifecycleQueries,
	mutate func(*agentpkg.RunConfig),
) triggerLifecycleFixture {
	t.Helper()

	providerRow := modelSelectionProviderRow(
		t,
		triggerLifecycleProviderID,
		string(models.ClientTypeOpenAICompletions),
		true,
	)
	modelRow := modelSelectionModelRow(
		t,
		triggerLifecycleModelID,
		"trigger-lifecycle-model",
		providerRow.ID,
		models.ModelTypeChat,
		true,
	)
	modelRow.Config = []byte(`{"context_window":` + strconv.Itoa(contextWindow) + `}`)
	queries := &triggerLifecycleQueries{
		modelSelectionFakeQueries: modelSelectionFakeQueries{
			models:   map[string]sqlc.Model{modelRow.ModelID: modelRow},
			provider: providerRow,
		},
		modelID: triggerLifecycleModelID,
	}
	provider := &triggerLifecycleProvider{}
	logger := slog.New(slog.DiscardHandler)
	applier := func(ctx context.Context, cfg agentpkg.RunConfig) (agentpkg.RunConfig, error) {
		if mutate != nil {
			mutate(&cfg)
		}
		out, err := contextview.ApplyProviderRunConfig(ctx, logger, cfg)
		if err != nil {
			return out, err
		}
		model := *out.Model
		model.Provider = provider
		out.Model = &model
		return out, nil
	}
	messages := &recordingMessageService{}
	runtime := &lifecycleSubagentRuntime{runID: runID}
	service := &Service{
		agent:             agentpkg.New(agentpkg.Deps{Logger: logger, ContextViewApplier: applier}),
		modelsService:     models.NewService(logger, queries),
		queries:           queries,
		contextLifecycles: lifecycles,
		messageService:    messages,
		settingsService:   settings.NewService(logger, queries, nil, nil),
		sessionRuntime:    runtime,
		clockLocation:     nil,
		logger:            logger,
	}
	return triggerLifecycleFixture{
		service:    service,
		runtime:    runtime,
		lifecycles: lifecycles,
		messages:   messages,
		provider:   provider,
		queries:    queries,
	}
}

func (f triggerLifecycleFixture) trigger(t *testing.T) (heartbeat.TriggerResult, error) {
	t.Helper()
	return f.service.TriggerHeartbeat(
		context.Background(),
		lifecycleTestBotID,
		heartbeat.TriggerPayload{
			BotID:           lifecycleTestBotID,
			Interval:        30,
			SessionID:       lifecycleTestSessionID,
			LastHeartbeatAt: triggerLifecyclePromptMarker,
		},
		"",
	)
}

func assertTriggerLifecycleRow(
	t *testing.T,
	lifecycles *recordingContextLifecycleQueries,
	runID, status, errorCode string,
) contextfrag.LifecycleSnapshot {
	t.Helper()
	if len(lifecycles.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(lifecycles.params))
	}
	row := lifecycles.params[0]
	if got := pgUUIDString(row.RunID); got != runID {
		t.Fatalf("context lifecycle run ID = %q, want admitted run ID %q", got, runID)
	}
	if row.Status != status {
		t.Fatalf("context lifecycle status = %q, want %q", row.Status, status)
	}
	if row.ErrorCode.String != errorCode || row.ErrorCode.Valid != (errorCode != "") {
		t.Fatalf("context lifecycle error code = %#v, want %q", row.ErrorCode, errorCode)
	}
	for _, privateText := range []string{
		triggerLifecyclePromptMarker,
		triggerLifecycleResponseMarker,
		"HEARTBEAT_OK",
	} {
		if bytes.Contains(row.Snapshot, []byte(privateText)) {
			t.Fatalf("content-light snapshot leaked %q: %s", privateText, row.Snapshot)
		}
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode lifecycle snapshot: %v", err)
	}
	if snapshot.Version != 1 {
		t.Fatalf("snapshot version = %d, want 1", snapshot.Version)
	}
	return snapshot
}

func TestTriggerHeartbeatPersistsCompletedLifecycleForAdmittedRun(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000911"
	fixture := newTriggerLifecycleFixture(
		t,
		admittedRunID,
		128000,
		&recordingContextLifecycleQueries{},
		nil,
	)

	result, err := fixture.trigger(t)
	if err != nil {
		t.Fatalf("TriggerHeartbeat() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerHeartbeat() status = %q, want ok", result.Status)
	}
	if fixture.provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", fixture.provider.callCount())
	}
	assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		admittedRunID,
		contextLifecycleStatusCompleted,
		"",
	)
	if len(fixture.messages.persisted) == 0 {
		t.Fatal("successful admitted heartbeat did not persist its round")
	}
	if len(fixture.runtime.finishes) != 1 || fixture.runtime.finishes[0].handle.RunID != admittedRunID {
		t.Fatalf("runtime finishes = %#v, want one finish for admitted run %q", fixture.runtime.finishes, admittedRunID)
	}
}

func TestDirectChatWithoutAdmissionMintsAndPersistsRunID(t *testing.T) {
	const unusedAdmissionRunID = "00000000-0000-4000-8000-000000000916"
	fixture := newTriggerLifecycleFixture(
		t,
		unusedAdmissionRunID,
		128000,
		&recordingContextLifecycleQueries{},
		nil,
	)

	response, err := fixture.service.Chat(
		context.Background(),
		ChatRequest{
			BotID:                lifecycleTestBotID,
			ChatID:               lifecycleTestBotID,
			ThreadID:             lifecycleTestSessionID,
			Query:                triggerLifecyclePromptMarker,
			UserMessagePersisted: true,
		},
	)
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if len(response.Messages) == 0 {
		t.Fatal("Chat() returned no assistant message")
	}
	if fixture.provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", fixture.provider.callCount())
	}
	if len(fixture.runtime.finishes) != 0 {
		t.Fatalf("direct Chat unexpectedly used durable admission: %#v", fixture.runtime.finishes)
	}
	if len(fixture.lifecycles.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(fixture.lifecycles.params))
	}
	row := fixture.lifecycles.params[0]
	runID := pgUUIDString(row.RunID)
	if _, err := uuid.Parse(runID); err != nil {
		t.Fatalf("direct Chat run ID = %q, want minted UUID: %v", runID, err)
	}
	if runID == unusedAdmissionRunID {
		t.Fatalf("direct Chat reused unrelated admission ID %q", runID)
	}
	assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		runID,
		contextLifecycleStatusCompleted,
		"",
	)
}

func TestAdmittedChatCancellationPersistsAbortedLifecycle(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000917"
	fixture := newTriggerLifecycleFixture(
		t,
		admittedRunID,
		128000,
		&recordingContextLifecycleQueries{unique: true},
		nil,
	)
	fixture.provider.nonClosing = true
	fixture.provider.streamStarted = make(chan struct{})

	handle, err := fixture.service.StartTurn(
		context.Background(),
		turn.StartTurnCommand{
			SchemaVersion:        1,
			TeamID:               "round5-team",
			Mode:                 turn.ModeChat,
			BotID:                lifecycleTestBotID,
			ChatID:               lifecycleTestBotID,
			ThreadID:             lifecycleTestSessionID,
			Query:                triggerLifecyclePromptMarker,
			UserMessagePersisted: true,
		},
	)
	if err != nil {
		t.Fatalf("StartTurn() error = %v", err)
	}
	select {
	case <-fixture.provider.streamStarted:
	case <-time.After(time.Second):
		t.Fatal("admitted chat did not reach the streaming provider")
	}
	handle.Cancel()
	for range handle.Events() {
	}
	for range handle.Errs() {
	}

	assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		admittedRunID,
		contextLifecycleStatusAborted,
		"",
	)
	if len(fixture.runtime.finishes) != 1 {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", fixture.runtime.finishes)
	}
	if got := fixture.runtime.finishes[0].status; got != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finish status = %q, want %q", got, sessionruntime.RunStatusAborted)
	}
}

func TestTriggerHeartbeatProviderBudgetFailurePersistsFailedBudgetWithoutAssistant(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000912"
	fixture := newTriggerLifecycleFixture(
		t,
		admittedRunID,
		1,
		&recordingContextLifecycleQueries{},
		nil,
	)

	_, err := fixture.trigger(t)
	if !errors.Is(err, contextfrag.ErrBudgetUnsatisfied) {
		t.Fatalf("TriggerHeartbeat() error = %v, want %v", err, contextfrag.ErrBudgetUnsatisfied)
	}
	if fixture.provider.callCount() != 0 {
		t.Fatalf("provider calls = %d, want 0 after provider-budget rejection", fixture.provider.callCount())
	}
	if len(fixture.messages.persisted) != 0 {
		t.Fatalf("persisted messages = %#v, want none after provider-budget rejection", fixture.messages.persisted)
	}
	snapshot := assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		admittedRunID,
		contextLifecycleStatusFailedBudget,
		string(apperror.CodeContextBudgetUnsatisfied),
	)
	if snapshot.BudgetPlan == nil || snapshot.BudgetPlan.Window != 1 {
		t.Fatalf("budget plan = %#v, want active provider plan with window 1", snapshot.BudgetPlan)
	}
	if !hasLifecycleMutation(snapshot, contextfrag.MutationContextBudgetFailure) {
		t.Fatalf("lifecycle mutations = %#v, want provider-budget failure", snapshot.Mutations)
	}
}

func TestTriggerHeartbeatResolveFailurePersistsAdmittedMinimalLifecycle(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000915"
	fixture := newTriggerLifecycleFixture(
		t,
		admittedRunID,
		128000,
		&recordingContextLifecycleQueries{},
		nil,
	)
	fixture.queries.settingsErr = errors.New("settings unavailable")

	_, err := fixture.trigger(t)
	if err == nil {
		t.Fatal("TriggerHeartbeat() error = nil, want model-resolution failure")
	}
	if fixture.provider.callCount() != 0 {
		t.Fatalf("provider calls = %d, want 0 after resolution failure", fixture.provider.callCount())
	}
	if len(fixture.messages.persisted) != 0 {
		t.Fatalf("persisted messages = %#v, want none after resolution failure", fixture.messages.persisted)
	}
	snapshot := assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		admittedRunID,
		contextLifecycleStatusFailedProvider,
		string(apperror.CodeOf(err)),
	)
	if len(snapshot.Breakdown) != 0 || len(snapshot.Mutations) != 0 || snapshot.BudgetPlan != nil {
		t.Fatalf("pre-context failure snapshot is not minimal: %#v", snapshot)
	}
	if len(fixture.runtime.finishes) != 1 ||
		fixture.runtime.finishes[0].status != sessionruntime.RunStatusErrored {
		t.Fatalf("runtime finishes = %#v, want one errored finish", fixture.runtime.finishes)
	}
}

func TestTriggerHeartbeatContextViewFallbackPersistsFallbackAndReachesProvider(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000913"
	fixture := newTriggerLifecycleFixture(
		t,
		admittedRunID,
		128000,
		&recordingContextLifecycleQueries{},
		func(cfg *agentpkg.RunConfig) {
			duplicate := func(role sdk.MessageRole, text string) contextfrag.ContextFrag {
				return contextfrag.MessageFrag(contextfrag.MessageFragInput{
					ID:      "forced-duplicate",
					Message: sdk.Message{Role: role, Content: []sdk.MessagePart{sdk.TextPart{Text: text}}},
					Kind:    contextfrag.KindConversationEvent,
					Slot:    contextfrag.SlotHistory,
					Source:  contextfrag.SourceRunConfig,
					Scope:   cfg.ContextScope,
				})
			}
			cfg.ContextSourceFrags = append(
				cfg.ContextSourceFrags,
				duplicate(sdk.MessageRoleUser, "first"),
				duplicate(sdk.MessageRoleAssistant, "second"),
			)
		},
	)

	result, err := fixture.trigger(t)
	if err != nil {
		t.Fatalf("TriggerHeartbeat() error = %v", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerHeartbeat() status = %q, want ok", result.Status)
	}
	if fixture.provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want fallback payload to reach provider once", fixture.provider.callCount())
	}
	snapshot := assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		admittedRunID,
		contextLifecycleStatusFallback,
		"",
	)
	if !hasLifecycleMutation(snapshot, contextfrag.MutationContextViewFallback) {
		t.Fatalf("lifecycle mutations = %#v, want context-view fallback", snapshot.Mutations)
	}
}

func TestTriggerHeartbeatLifecycleStoreFailureDoesNotFailSuccessfulTrigger(t *testing.T) {
	const admittedRunID = "00000000-0000-4000-8000-000000000914"
	storeErr := errors.New("context lifecycle store unavailable")
	fixture := newTriggerLifecycleFixture(
		t,
		admittedRunID,
		128000,
		&recordingContextLifecycleQueries{err: storeErr},
		nil,
	)

	result, err := fixture.trigger(t)
	if err != nil {
		t.Fatalf("TriggerHeartbeat() error = %v, want nil despite lifecycle store failure", err)
	}
	if result.Status != "ok" {
		t.Fatalf("TriggerHeartbeat() status = %q, want ok", result.Status)
	}
	if fixture.provider.callCount() != 1 {
		t.Fatalf("provider calls = %d, want 1", fixture.provider.callCount())
	}
	assertTriggerLifecycleRow(
		t,
		fixture.lifecycles,
		admittedRunID,
		contextLifecycleStatusCompleted,
		"",
	)
	if got := fixture.service.contextLifecyclePersistenceErrors.Load(); got != 1 {
		t.Fatalf("lifecycle persistence error count = %d, want 1", got)
	}
}

func hasLifecycleMutation(
	snapshot contextfrag.LifecycleSnapshot,
	kind contextfrag.MutationKind,
) bool {
	for _, mutation := range snapshot.Mutations {
		if mutation.Kind == kind {
			return true
		}
	}
	return false
}
