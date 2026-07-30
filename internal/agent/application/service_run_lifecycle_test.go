package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	tools "github.com/memohai/memoh/internal/agent/tool"
	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

const (
	lifecycleTestRunID     = "11111111-1111-4111-8111-111111111111"
	lifecycleTestBotID     = "22222222-2222-4222-8222-222222222222"
	lifecycleTestSessionID = "33333333-3333-4333-8333-333333333333"
)

type lifecycleSubagentRuntime struct {
	runID    string
	cancel   context.CancelFunc
	finishes []recordedFinish
}

func (r *lifecycleSubagentRuntime) Admit(_ context.Context, input sessionruntime.AdmitInput) (sessionruntime.Admission, error) {
	r.cancel = input.Execution.Cancel
	return sessionruntime.Admission{
		RunID:   r.runID,
		Started: true,
		Handle: sessionruntime.RunHandle{
			BotID:        input.BotID,
			SessionID:    input.SessionID,
			RunID:        r.runID,
			FencingToken: 1,
		},
	}, nil
}

func (r *lifecycleSubagentRuntime) FinishRun(
	_ context.Context,
	handle sessionruntime.RunHandle,
	status, message string,
) error {
	r.finishes = append(r.finishes, recordedFinish{
		handle:  handle,
		status:  status,
		message: message,
	})
	return nil
}

type recordingContextLifecycleQueries struct {
	dbstore.Queries
	params      []sqlc.CreateContextLifecycleParams
	err         error
	existing    *sqlc.ContextLifecycle
	getErr      error
	metadata    []byte
	metadataErr error
}

func (q *recordingContextLifecycleQueries) CreateContextLifecycle(
	_ context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.ContextLifecycle, error) {
	q.params = append(q.params, arg)
	return sqlc.ContextLifecycle{}, q.err
}

func (q *recordingContextLifecycleQueries) GetContextLifecycleByRunID(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.ContextLifecycle, error) {
	if q.getErr != nil {
		return sqlc.ContextLifecycle{}, q.getErr
	}
	if q.existing == nil {
		return sqlc.ContextLifecycle{}, pgx.ErrNoRows
	}
	return *q.existing, nil
}

func (q *recordingContextLifecycleQueries) GetLatestAssistantContextLifecycleMetadataByRunID(
	_ context.Context,
	_ pgtype.UUID,
) ([]byte, error) {
	if q.metadataErr != nil {
		return nil, q.metadataErr
	}
	if q.metadata == nil {
		return nil, pgx.ErrNoRows
	}
	return q.metadata, nil
}

func lifecycleTestRunConfig(t *testing.T, runID string, mutations ...contextfrag.MutationRecord) native.RunConfig {
	t.Helper()
	ledger := contextfrag.NewMutationLedger()
	for _, mutation := range mutations {
		ledger.Record(mutation.Kind, mutation.Detail)
	}
	manifest := contextfrag.BuildManifest(nil)
	manifest.Mutations = ledger
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(manifest)
	return native.RunConfig{
		RunID: runID,
		Identity: native.SessionContext{
			BotID:     lifecycleTestBotID,
			SessionID: lifecycleTestSessionID,
		},
		ContextLifecycle: holder,
	}
}

func TestContextLifecycleTerminalWritesAdmittedRunExactlyOnce(t *testing.T) {
	queries := &recordingContextLifecycleQueries{}
	service := &Service{contextLifecycles: queries}
	terminal := service.contextLifecycleTerminal(
		context.Background(),
		lifecycleTestRunConfig(t, lifecycleTestRunID),
	)

	terminal(nil)
	terminal(errors.New("late duplicate"))

	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	if got := pgUUIDString(queries.params[0].RunID); got != lifecycleTestRunID {
		t.Fatalf("persisted run ID = %q, want admitted ID %q", got, lifecycleTestRunID)
	}
	if got := queries.params[0].Status; got != contextLifecycleStatusCompleted {
		t.Fatalf("status = %q, want %q", got, contextLifecycleStatusCompleted)
	}
}

func TestContextLifecycleTerminalClassifiesTerminalState(t *testing.T) {
	tests := []struct {
		name      string
		mutations []contextfrag.MutationRecord
		cause     error
		status    string
		errorCode string
	}{
		{
			name:   "completed",
			status: contextLifecycleStatusCompleted,
		},
		{
			name: "budget failure",
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "protected_context_overflow",
			}},
			cause:     fmt.Errorf("private detail: %w", contextfrag.ErrProtectedContextOverflow),
			status:    contextLifecycleStatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
		{
			name: "legacy fallback",
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextViewFallback,
				Detail: "collector_error",
			}},
			status: contextLifecycleStatusFallback,
		},
		{
			name:   "provider failure",
			cause:  errors.New("private upstream failure"),
			status: contextLifecycleStatusFailedProvider,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := &recordingContextLifecycleQueries{}
			service := &Service{contextLifecycles: queries}
			service.contextLifecycleTerminal(
				context.Background(),
				lifecycleTestRunConfig(t, lifecycleTestRunID, tt.mutations...),
			)(tt.cause)

			if len(queries.params) != 1 {
				t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
			}
			got := queries.params[0]
			if got.Status != tt.status {
				t.Fatalf("status = %q, want %q", got.Status, tt.status)
			}
			if got.ErrorCode.String != tt.errorCode || got.ErrorCode.Valid != (tt.errorCode != "") {
				t.Fatalf("error code = %#v, want %q", got.ErrorCode, tt.errorCode)
			}
		})
	}
}

func TestContextLifecycleTerminalMintsForDirectRunAndKeepsSnapshotContentLight(t *testing.T) {
	const privatePrompt = "PRIVATE PROMPT CONTENT MUST NOT PERSIST"
	queries := &recordingContextLifecycleQueries{}
	service := &Service{contextLifecycles: queries}
	cfg := lifecycleTestRunConfig(t, runIDForChatRequest(""))
	cfg.System = privatePrompt
	cfg.Query = privatePrompt

	service.contextLifecycleTerminal(context.Background(), cfg)(nil)

	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	if _, err := uuid.Parse(pgUUIDString(queries.params[0].RunID)); err != nil {
		t.Fatalf("direct run ID is not a UUID: %v", err)
	}
	if bytes.Contains(queries.params[0].Snapshot, []byte(privatePrompt)) {
		t.Fatalf("snapshot leaked raw prompt text: %s", queries.params[0].Snapshot)
	}
}

func TestContextLifecycleTerminalHeartbeatUsesAdmittedRunID(t *testing.T) {
	const admittedHeartbeatRunID = "44444444-4444-4444-8444-444444444444"
	queries := &recordingContextLifecycleQueries{}
	service := &Service{contextLifecycles: queries}
	cfg := lifecycleTestRunConfig(t, admittedHeartbeatRunID)
	cfg.SessionType = "heartbeat"

	service.contextLifecycleTerminal(context.Background(), cfg)(nil)

	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	if got := pgUUIDString(queries.params[0].RunID); got != admittedHeartbeatRunID {
		t.Fatalf("heartbeat run ID = %q, want admitted ID %q", got, admittedHeartbeatRunID)
	}
}

func TestSubagentTerminalPersistsFinalSnapshotBeforeFinishingRunExactlyOnce(t *testing.T) {
	const admittedRunID = "55555555-5555-4555-8555-555555555555"
	runtime := &lifecycleSubagentRuntime{runID: admittedRunID}
	queries := &recordingContextLifecycleQueries{}
	service := &Service{
		sessionRuntime:    runtime,
		contextLifecycles: queries,
	}
	runCtx, runID, terminal, err := service.AdmitSubagentRun(
		context.Background(),
		lifecycleTestBotID,
		lifecycleTestSessionID,
		"subagent:task-1",
		[]byte(`{"message":"work"}`),
	)
	if err != nil {
		t.Fatalf("AdmitSubagentRun error: %v", err)
	}
	if runCtx == nil {
		t.Fatal("AdmitSubagentRun returned nil run context")
	}
	if runID != admittedRunID {
		t.Fatalf("run ID = %q, want admitted RunID %q", runID, admittedRunID)
	}
	cfg := lifecycleTestRunConfig(t, admittedRunID)
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}

	terminal(tools.SubagentTerminal{ContextLifecycle: &snapshot})
	terminal(tools.SubagentTerminal{
		Cause:            errors.New("late duplicate"),
		ContextLifecycle: &contextfrag.LifecycleSnapshot{Version: 99},
	})

	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	if got := pgUUIDString(queries.params[0].RunID); got != admittedRunID {
		t.Fatalf("persisted RunID = %q, want admitted RunID %q", got, admittedRunID)
	}
	if len(runtime.finishes) != 1 {
		t.Fatalf("FinishRun calls = %d, want 1", len(runtime.finishes))
	}
	if got := runtime.finishes[0].status; got != sessionruntime.RunStatusCompleted {
		t.Fatalf("FinishRun status = %q, want %q", got, sessionruntime.RunStatusCompleted)
	}
}

func TestCanceledSubagentPersistsFailedLifecycleAndFinishesAborted(t *testing.T) {
	const admittedRunID = "66666666-6666-4666-8666-666666666666"
	runtime := &lifecycleSubagentRuntime{runID: admittedRunID}
	queries := &recordingContextLifecycleQueries{}
	service := &Service{
		sessionRuntime:    runtime,
		contextLifecycles: queries,
	}
	runCtx, _, terminal, err := service.AdmitSubagentRun(
		context.Background(),
		lifecycleTestBotID,
		lifecycleTestSessionID,
		"subagent:task-canceled",
		[]byte(`{"message":"work"}`),
	)
	if err != nil {
		t.Fatalf("AdmitSubagentRun error: %v", err)
	}
	cfg := lifecycleTestRunConfig(t, admittedRunID)
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}

	runtime.cancel()
	<-runCtx.Done()
	terminal(tools.SubagentTerminal{ContextLifecycle: &snapshot})

	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	if got := queries.params[0].Status; got != contextLifecycleStatusFailedProvider {
		t.Fatalf("lifecycle status = %q, want %q", got, contextLifecycleStatusFailedProvider)
	}
	if len(runtime.finishes) != 1 {
		t.Fatalf("FinishRun calls = %d, want 1", len(runtime.finishes))
	}
	if got := runtime.finishes[0].status; got != sessionruntime.RunStatusAborted {
		t.Fatalf("FinishRun status = %q, want %q", got, sessionruntime.RunStatusAborted)
	}
	if got := runtime.finishes[0].message; got != "" {
		t.Fatalf("FinishRun message = %q, want empty deliberate-cancel cause", got)
	}
}

func TestContextLifecycleStoreErrorIsCountedAndNotReturned(t *testing.T) {
	queries := &recordingContextLifecycleQueries{err: errors.New("database unavailable")}
	service := &Service{contextLifecycles: queries}

	service.contextLifecycleTerminal(
		context.Background(),
		lifecycleTestRunConfig(t, lifecycleTestRunID),
	)(nil)

	if got := service.contextLifecyclePersistenceErrors.Load(); got != 1 {
		t.Fatalf("persistence error count = %d, want 1", got)
	}
}

func TestRecoverContextLifecycleFromAssistantMetadataWritesFailedRun(t *testing.T) {
	cfg := lifecycleTestRunConfig(t, lifecycleTestRunID)
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	metadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: snapshot,
	})
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	queries := &recordingContextLifecycleQueries{metadata: metadata}
	service := &Service{contextLifecycles: queries}

	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		errors.New("continuation failed before resume"),
	)

	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	if got := pgUUIDString(queries.params[0].RunID); got != lifecycleTestRunID {
		t.Fatalf("persisted run ID = %q, want %q", got, lifecycleTestRunID)
	}
	if got := queries.params[0].Status; got != contextLifecycleStatusFailedProvider {
		t.Fatalf("status = %q, want %q", got, contextLifecycleStatusFailedProvider)
	}
}

func TestRecoverContextLifecycleFromAssistantMetadataDoesNotDuplicateExistingRow(t *testing.T) {
	queries := &recordingContextLifecycleQueries{existing: &sqlc.ContextLifecycle{}}
	service := &Service{contextLifecycles: queries}

	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		errors.New("late continuation error"),
	)

	if len(queries.params) != 0 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 0", len(queries.params))
	}
}

func TestRecoverContextLifecycleStoreErrorIsCountedAndNotReturned(t *testing.T) {
	queries := &recordingContextLifecycleQueries{getErr: errors.New("database unavailable")}
	service := &Service{contextLifecycles: queries}

	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		errors.New("continuation failed"),
	)

	if got := service.contextLifecyclePersistenceErrors.Load(); got != 1 {
		t.Fatalf("persistence error count = %d, want 1", got)
	}
	if len(queries.params) != 0 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 0", len(queries.params))
	}
}
