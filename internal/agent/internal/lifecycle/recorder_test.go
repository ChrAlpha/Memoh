package lifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/internal/agenttest"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

func TestContextLifecycleTerminalWritesAdmittedRunExactlyOnce(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	terminal := service.Terminal(context.Background(), agenttest.LifecycleRunConfig())

	terminal(nil)
	terminal(apperror.New(apperror.CodeWorkspaceUnreachable, nil))

	if len(store.Creates) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
	}
	got := store.Creates[0]
	if runreq.PGUUIDString(got.RunID) != agenttest.LifecycleRunID {
		t.Fatalf("persisted run ID = %q, want %q", runreq.PGUUIDString(got.RunID), agenttest.LifecycleRunID)
	}
	if got.Status != StatusCompleted || got.ErrorCode.Valid {
		t.Fatalf("terminal = (%q, %#v), want completed without error code", got.Status, got.ErrorCode)
	}
	if bytes.Contains(got.Snapshot, []byte("private-content-marker")) || bytes.Contains(got.Snapshot, []byte(`"items"`)) {
		t.Fatalf("content-light snapshot leaked manifest items: %s", got.Snapshot)
	}
}

func TestContextLifecycleTerminalClassifiesTerminalState(t *testing.T) {
	canceledCtx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)

	tests := []struct {
		name      string
		ctx       context.Context
		mutations []contextfrag.MutationRecord
		cause     error
		status    string
		errorCode string
	}{
		{name: "completed", ctx: context.Background(), status: StatusCompleted},
		{
			name:      "provider failure with stable code",
			ctx:       context.Background(),
			cause:     apperror.New(apperror.CodeWorkspaceUnreachable, nil),
			status:    StatusFailedProvider,
			errorCode: string(apperror.CodeWorkspaceUnreachable),
		},
		{
			name: "budget failure mutation",
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "protected_context_overflow",
			}},
			cause:     errors.New("private detail"),
			status:    StatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
		{
			name: "budget failure mutation without error",
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "budget_unsatisfied",
			}},
			status:    StatusFailedBudget,
			errorCode: string(apperror.CodeContextBudgetUnsatisfied),
		},
		{
			name:      "stable protected overflow code without sentinel",
			cause:     apperror.New(apperror.CodeContextProtectedOverflow, nil),
			status:    StatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
		{
			name:   "provider cancellation while owner remains active",
			ctx:    context.Background(),
			cause:  context.Canceled,
			status: StatusFailedProvider,
		},
		{
			name:   "explicit abort",
			ctx:    canceledCtx,
			cause:  context.Canceled,
			status: StatusAborted,
		},
		{
			name:   "explicit abort behind private application cause",
			ctx:    canceledCtx,
			cause:  apperror.Wrap(apperror.CodeWorkspaceUnreachable, context.Canceled, nil),
			status: StatusAborted,
		},
		{
			name: "fallback",
			ctx:  context.Background(),
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextViewFallback,
				Detail: "collector_error",
			}},
			status: StatusFallback,
		},
		{
			name: "provider failure outranks fallback",
			ctx:  context.Background(),
			mutations: []contextfrag.MutationRecord{{
				Kind: contextfrag.MutationContextViewFallback,
			}},
			cause:  errors.New("private upstream failure"),
			status: StatusFailedProvider,
		},
		{
			name: "explicit abort outranks fallback",
			ctx:  canceledCtx,
			mutations: []contextfrag.MutationRecord{{
				Kind: contextfrag.MutationContextViewFallback,
			}},
			cause:  context.Canceled,
			status: StatusAborted,
		},
		{
			name: "budget failure outranks explicit abort",
			ctx:  canceledCtx,
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "protected_context_overflow",
			}},
			cause:     context.Canceled,
			status:    StatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &agenttest.RecordingLifecycleStore{}
			service := New(Deps{Store: store})
			service.Terminal(tt.ctx, agenttest.LifecycleRunConfig(tt.mutations...))(tt.cause)

			if len(store.Creates) != 1 {
				t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
			}
			got := store.Creates[0]
			if got.Status != tt.status || got.ErrorCode.String != tt.errorCode || got.ErrorCode.Valid != (tt.errorCode != "") {
				t.Fatalf("terminal classification = (%q, %#v), want (%q, %q)", got.Status, got.ErrorCode, tt.status, tt.errorCode)
			}
		})
	}
}

func TestEnsureTerminalContextLifecycleCreatesMinimalFallbackOnlyWhenMissing(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	cause := apperror.New(apperror.CodeWorkspaceUnreachable, nil)

	service.EnsureTerminal(
		context.Background(),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		cause,
	)

	if store.GetCalls != 1 || len(store.Creates) != 1 {
		t.Fatalf("fallback calls = get %d, create %d; want 1, 1", store.GetCalls, len(store.Creates))
	}
	row := store.Creates[0]
	if row.Status != StatusFailedProvider || row.ErrorCode.String != string(apperror.CodeWorkspaceUnreachable) {
		t.Fatalf("fallback terminal = (%q, %#v)", row.Status, row.ErrorCode)
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode fallback snapshot: %v", err)
	}
	if snapshot.Version != contextfrag.LifecycleSnapshotVersion || snapshot.Counts != (contextfrag.ManifestCounts{}) {
		t.Fatalf("minimal fallback snapshot = %#v", snapshot)
	}

	store.Creates = nil
	runUUID, botUUID, sessionUUID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	store.Existing = &sqlc.GetContextLifecycleByRunIDRow{RunID: runUUID, BotID: botUUID, SessionID: sessionUUID}
	service.EnsureTerminal(
		context.Background(),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		cause,
	)
	if len(store.Creates) != 0 {
		t.Fatalf("existing authoritative lifecycle was replaced: %#v", store.Creates)
	}
}

func TestContextLifecycleStoreFailureIsCountedAndDoesNotEscape(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{CreateErr: errors.New("database unavailable")}
	service := New(Deps{Store: store})

	service.Terminal(context.Background(), agenttest.LifecycleRunConfig())(nil)

	if got := service.PersistenceErrors(); got != 1 {
		t.Fatalf("persistence failure count = %d, want 1", got)
	}
}

func TestAuthoritativeSnapshotReplacesOnlyRecoveredAbortedFallback(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{
		CreateErr: &pgconn.PgError{Code: "23505"},
		Existing: &sqlc.GetContextLifecycleByRunIDRow{
			RunID: runUUID, BotID: botUUID, SessionID: sessionUUID,
			Status: StatusAborted,
		},
	}
	service := New(Deps{Store: store})

	service.Terminal(context.Background(), agenttest.LifecycleRunConfig())(nil)

	if len(store.Updates) != 1 {
		t.Fatalf("aborted snapshot updates = %d, want 1", len(store.Updates))
	}
}

func TestRecoverContextLifecycleFromAssistantMetadata(t *testing.T) {
	snapshot, ok := agenttest.LifecycleRunConfig().ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	metadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	const assistantMessageID = "44444444-4444-4444-8444-444444444444"
	store := &agenttest.RecordingLifecycleStore{
		AssistantID: agenttest.UUID(assistantMessageID),
		Metadata:    metadata,
	}
	service := New(Deps{Store: store})

	service.recoverFromAssistantMetadata(
		context.Background(),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		apperror.New(apperror.CodeWorkspaceUnreachable, nil),
	)

	if len(store.Creates) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
	}
	row := store.Creates[0]
	if row.Status != StatusFailedProvider ||
		!row.ErrorCode.Valid || row.ErrorCode.String != string(apperror.CodeWorkspaceUnreachable) {
		t.Fatalf("recovered terminal = (%q, %#v), want failed_provider with stable code", row.Status, row.ErrorCode)
	}
	var recovered contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.AssistantMessageID != assistantMessageID {
		t.Fatalf("assistant message ID = %q, want %q", recovered.AssistantMessageID, assistantMessageID)
	}
}

func TestRecoverContextLifecycleFromAssistantMetadataSkipsExistingOrUnavailable(t *testing.T) {
	runID, botID, sessionID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		store *agenttest.RecordingLifecycleStore
	}{
		{
			name: "existing lifecycle",
			store: &agenttest.RecordingLifecycleStore{Existing: &sqlc.GetContextLifecycleByRunIDRow{
				RunID: runID, BotID: botID, SessionID: sessionID,
			}},
		},
		{name: "missing assistant metadata", store: &agenttest.RecordingLifecycleStore{}},
		{name: "malformed assistant metadata", store: &agenttest.RecordingLifecycleStore{Metadata: []byte(`{"context_lifecycle":"invalid"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := New(Deps{Store: tt.store})
			service.recoverFromAssistantMetadata(
				context.Background(),
				agenttest.LifecycleRunID,
				agenttest.LifecycleBotID,
				agenttest.LifecycleSessionID,
				errors.New("continuation failed"),
			)
			if len(tt.store.Creates) != 0 || service.PersistenceErrors() != 0 {
				t.Fatalf("recovery writes = %d, failures = %d; want no-op", len(tt.store.Creates), service.PersistenceErrors())
			}
		})
	}
}

func TestRecoverContextLifecycleFromAssistantMetadataCountsStoreErrorsAndSkipsOwnershipLoss(t *testing.T) {
	metadataErr := errors.New("metadata unavailable")
	store := &agenttest.RecordingLifecycleStore{MetadataErr: metadataErr}
	service := New(Deps{Store: store})
	service.recoverFromAssistantMetadata(
		context.Background(),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		errors.New("continuation failed"),
	)
	if got := service.PersistenceErrors(); got != 1 {
		t.Fatalf("persistence failure count = %d, want 1", got)
	}

	ownershipStore := &agenttest.RecordingLifecycleStore{}
	service = New(Deps{Store: ownershipStore})
	service.recoverFromAssistantMetadata(
		context.Background(),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		sessionruntime.ErrRunOwnershipLost,
	)
	if ownershipStore.GetCalls != 0 || len(ownershipStore.Creates) != 0 {
		t.Fatalf("ownership-lost recovery touched store: gets=%d creates=%d", ownershipStore.GetCalls, len(ownershipStore.Creates))
	}
}

func TestBudgetOverAbortKeepsAbortTraceInSnapshot(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		mutations []contextfrag.MutationRecord
		cause     error
		status    string
		wantTrace bool
	}{
		{
			name: "budget win over explicit abort records the abort trace",
			ctx:  canceledCtx,
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "protected_context_overflow",
			}},
			cause:     context.Canceled,
			status:    StatusFailedBudget,
			wantTrace: true,
		},
		{
			name: "budget failure without cancellation stays trace-free",
			ctx:  context.Background(),
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "budget_unsatisfied",
			}},
			cause:     errors.New("private detail"),
			status:    StatusFailedBudget,
			wantTrace: false,
		},
		{
			name:      "plain abort needs no trace",
			ctx:       canceledCtx,
			cause:     context.Canceled,
			status:    StatusAborted,
			wantTrace: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &agenttest.RecordingLifecycleStore{}
			service := New(Deps{Store: store})
			service.Terminal(tt.ctx, agenttest.LifecycleRunConfig(tt.mutations...))(tt.cause)

			if len(store.Creates) != 1 {
				t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
			}
			got := store.Creates[0]
			if got.Status != tt.status {
				t.Fatalf("status = %q, want %q", got.Status, tt.status)
			}
			var snapshot contextfrag.LifecycleSnapshot
			if err := json.Unmarshal(got.Snapshot, &snapshot); err != nil {
				t.Fatalf("unmarshal snapshot: %v", err)
			}
			hasTrace := false
			for _, mutation := range snapshot.Mutations {
				if mutation.Kind == contextfrag.MutationRunAbortObserved {
					hasTrace = true
				}
			}
			if hasTrace != tt.wantTrace {
				t.Fatalf("abort trace = %v, want %v (mutations %#v)", hasTrace, tt.wantTrace, snapshot.Mutations)
			}
		})
	}
}
