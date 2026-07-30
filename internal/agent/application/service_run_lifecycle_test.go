package application

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/memohai/memoh/internal/db/store"
)

const (
	lifecycleTestRunID     = "11111111-1111-4111-8111-111111111111"
	lifecycleTestBotID     = "22222222-2222-4222-8222-222222222222"
	lifecycleTestSessionID = "33333333-3333-4333-8333-333333333333"
)

type recordingContextLifecycleQueries struct {
	dbstore.Queries
	params []sqlc.CreateContextLifecycleParams
	err    error
}

func (q *recordingContextLifecycleQueries) CreateContextLifecycle(
	_ context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.ContextLifecycle, error) {
	q.params = append(q.params, arg)
	return sqlc.ContextLifecycle{}, q.err
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
