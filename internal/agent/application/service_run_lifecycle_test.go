package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

const (
	lifecycleTestRunID     = "11111111-1111-4111-8111-111111111111"
	lifecycleTestBotID     = "22222222-2222-4222-8222-222222222222"
	lifecycleTestSessionID = "33333333-3333-4333-8333-333333333333"
)

func lifecycleTestRunConfig(mutations ...contextfrag.MutationRecord) native.RunConfig {
	ledger := contextfrag.NewMutationLedger()
	for _, mutation := range mutations {
		ledger.Record(mutation.Kind, mutation.Detail)
	}
	holder := contextfrag.NewLifecycleHolder()
	holder.SetManifest(contextfrag.Manifest{
		View: contextfrag.ViewRunConfigPreProvider,
		Counts: contextfrag.ManifestCounts{
			Fragments: 2,
			Messages:  1,
			TextBytes: 64,
		},
		Items:     []contextfrag.ManifestItem{{ID: "private-content-marker"}},
		Mutations: ledger,
	})
	return native.RunConfig{
		RunID: lifecycleTestRunID,
		Identity: native.SessionContext{
			BotID:     lifecycleTestBotID,
			SessionID: lifecycleTestSessionID,
		},
		ContextLifecycle: holder,
	}
}

func TestContextLifecycleTerminalWritesAdmittedRunExactlyOnce(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := &Service{contextLifecycles: store}
	terminal := service.contextLifecycleTerminal(context.Background(), lifecycleTestRunConfig())

	terminal(nil)
	terminal(apperror.New(apperror.CodeWorkspaceUnreachable, nil))

	if len(store.Creates) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
	}
	got := store.Creates[0]
	if runreq.PGUUIDString(got.RunID) != lifecycleTestRunID {
		t.Fatalf("persisted run ID = %q, want %q", runreq.PGUUIDString(got.RunID), lifecycleTestRunID)
	}
	if got.Status != contextLifecycleStatusCompleted || got.ErrorCode.Valid {
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
		{name: "completed", ctx: context.Background(), status: contextLifecycleStatusCompleted},
		{
			name:      "provider failure with stable code",
			ctx:       context.Background(),
			cause:     apperror.New(apperror.CodeWorkspaceUnreachable, nil),
			status:    contextLifecycleStatusFailedProvider,
			errorCode: string(apperror.CodeWorkspaceUnreachable),
		},
		{
			name: "budget failure mutation",
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "protected_context_overflow",
			}},
			cause:     errors.New("private detail"),
			status:    contextLifecycleStatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
		{
			name: "budget failure mutation without error",
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "budget_unsatisfied",
			}},
			status:    contextLifecycleStatusFailedBudget,
			errorCode: string(apperror.CodeContextBudgetUnsatisfied),
		},
		{
			name:      "stable protected overflow code without sentinel",
			cause:     apperror.New(apperror.CodeContextProtectedOverflow, nil),
			status:    contextLifecycleStatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
		{
			name:   "provider cancellation while owner remains active",
			ctx:    context.Background(),
			cause:  context.Canceled,
			status: contextLifecycleStatusFailedProvider,
		},
		{
			name:   "explicit abort",
			ctx:    canceledCtx,
			cause:  context.Canceled,
			status: contextLifecycleStatusAborted,
		},
		{
			name:   "explicit abort behind private application cause",
			ctx:    canceledCtx,
			cause:  apperror.Wrap(apperror.CodeWorkspaceUnreachable, context.Canceled, nil),
			status: contextLifecycleStatusAborted,
		},
		{
			name: "fallback",
			ctx:  context.Background(),
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextViewFallback,
				Detail: "collector_error",
			}},
			status: contextLifecycleStatusFallback,
		},
		{
			name: "provider failure outranks fallback",
			ctx:  context.Background(),
			mutations: []contextfrag.MutationRecord{{
				Kind: contextfrag.MutationContextViewFallback,
			}},
			cause:  errors.New("private upstream failure"),
			status: contextLifecycleStatusFailedProvider,
		},
		{
			name: "explicit abort outranks fallback",
			ctx:  canceledCtx,
			mutations: []contextfrag.MutationRecord{{
				Kind: contextfrag.MutationContextViewFallback,
			}},
			cause:  context.Canceled,
			status: contextLifecycleStatusAborted,
		},
		{
			name: "budget failure outranks explicit abort",
			ctx:  canceledCtx,
			mutations: []contextfrag.MutationRecord{{
				Kind:   contextfrag.MutationContextBudgetFailure,
				Detail: "protected_context_overflow",
			}},
			cause:     context.Canceled,
			status:    contextLifecycleStatusFailedBudget,
			errorCode: string(apperror.CodeContextProtectedOverflow),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &agenttest.RecordingLifecycleStore{}
			service := &Service{contextLifecycles: store}
			service.contextLifecycleTerminal(tt.ctx, lifecycleTestRunConfig(tt.mutations...))(tt.cause)

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
	service := &Service{contextLifecycles: store}
	cause := apperror.New(apperror.CodeWorkspaceUnreachable, nil)

	service.EnsureTerminalContextLifecycle(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		cause,
	)

	if store.GetCalls != 1 || len(store.Creates) != 1 {
		t.Fatalf("fallback calls = get %d, create %d; want 1, 1", store.GetCalls, len(store.Creates))
	}
	row := store.Creates[0]
	if row.Status != contextLifecycleStatusFailedProvider || row.ErrorCode.String != string(apperror.CodeWorkspaceUnreachable) {
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
	runUUID, botUUID, sessionUUID, err := parseContextLifecycleIDs(
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	store.Existing = &sqlc.GetContextLifecycleByRunIDRow{RunID: runUUID, BotID: botUUID, SessionID: sessionUUID}
	service.EnsureTerminalContextLifecycle(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		cause,
	)
	if len(store.Creates) != 0 {
		t.Fatalf("existing authoritative lifecycle was replaced: %#v", store.Creates)
	}
}

func TestContextLifecycleStoreFailureIsCountedAndDoesNotEscape(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{CreateErr: errors.New("database unavailable")}
	service := &Service{contextLifecycles: store}

	service.contextLifecycleTerminal(context.Background(), lifecycleTestRunConfig())(nil)

	if got := service.contextLifecyclePersistenceErrors.Load(); got != 1 {
		t.Fatalf("persistence failure count = %d, want 1", got)
	}
}

func TestAuthoritativeSnapshotReplacesOnlyRecoveredAbortedFallback(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseContextLifecycleIDs(
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{
		CreateErr: &pgconn.PgError{Code: "23505"},
		Existing: &sqlc.GetContextLifecycleByRunIDRow{
			RunID: runUUID, BotID: botUUID, SessionID: sessionUUID,
			Status: contextLifecycleStatusAborted,
		},
	}
	service := &Service{contextLifecycles: store}

	service.contextLifecycleTerminal(context.Background(), lifecycleTestRunConfig())(nil)

	if len(store.Updates) != 1 {
		t.Fatalf("aborted snapshot updates = %d, want 1", len(store.Updates))
	}
}

func TestTurnRunFinisherCreatesFallbackOnlyAfterFencedTerminalFinish(t *testing.T) {
	abortedCtx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)
	tests := []struct {
		name        string
		ctx         context.Context
		status      string
		cause       error
		finishErr   error
		wantCreates int
		wantStatus  string
	}{
		{
			name:        "pre-context failure",
			ctx:         context.Background(),
			status:      sessionruntime.RunStatusErrored,
			cause:       apperror.New(apperror.CodeWorkspaceUnreachable, nil),
			wantCreates: 1,
			wantStatus:  contextLifecycleStatusFailedProvider,
		},
		{name: "decision pause", ctx: context.Background()},
		{
			name:        "explicit abort",
			ctx:         abortedCtx,
			status:      sessionruntime.RunStatusAborted,
			wantCreates: 1,
			wantStatus:  contextLifecycleStatusAborted,
		},
		{
			name:      "lost fence",
			ctx:       context.Background(),
			status:    sessionruntime.RunStatusErrored,
			cause:     errors.New("late failure"),
			finishErr: sessionruntime.ErrRunOwnershipLost,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admitter := &agenttest.LifecycleAdmitter{FinishErr: tt.finishErr}
			store := &agenttest.RecordingLifecycleStore{}
			service := &Service{sessionRuntime: admitter, contextLifecycles: store}
			admission := lifecycleTestAdmission()

			service.turnRunFinisher(tt.ctx, admission, sessionmode.Chat)(outcome.RunOutcome{Status: tt.status, Cause: tt.cause})

			if len(admitter.Finishes) != 1 {
				t.Fatalf("FinishRun calls = %d, want 1", len(admitter.Finishes))
			}
			if tt.name == "pre-context failure" && admitter.Finishes[0].Message != string(apperror.CodeWorkspaceUnreachable) {
				t.Fatalf("stable finish code = %q", admitter.Finishes[0].Message)
			}
			if len(store.Creates) != tt.wantCreates {
				t.Fatalf("lifecycle creates = %d, want %d", len(store.Creates), tt.wantCreates)
			}
			if tt.wantCreates > 0 && store.Creates[0].Status != tt.wantStatus {
				t.Fatalf("lifecycle status = %q, want %q", store.Creates[0].Status, tt.wantStatus)
			}
		})
	}
}

func TestSubagentTerminalPersistsSnapshotAndPreContextFallbackExactlyOnce(t *testing.T) {
	tests := []struct {
		name         string
		terminal     func() tools.SubagentTerminal
		wantStatus   string
		wantFinish   string
		wantSnapshot contextfrag.LifecycleSnapshot
	}{
		{
			name: "final snapshot",
			terminal: func() tools.SubagentTerminal {
				snapshot, ok := lifecycleTestRunConfig().ContextLifecycle.Snapshot()
				if !ok {
					t.Fatal("test lifecycle snapshot is unavailable")
				}
				return tools.SubagentTerminal{ContextLifecycle: &snapshot}
			},
			wantStatus:   contextLifecycleStatusCompleted,
			wantFinish:   sessionruntime.RunStatusCompleted,
			wantSnapshot: contextfrag.LifecycleSnapshot{Version: contextfrag.LifecycleSnapshotVersion},
		},
		{
			name: "failure before context",
			terminal: func() tools.SubagentTerminal {
				return tools.SubagentTerminal{Cause: apperror.New(apperror.CodeWorkspaceUnreachable, nil)}
			},
			wantStatus:   contextLifecycleStatusFailedProvider,
			wantFinish:   sessionruntime.RunStatusErrored,
			wantSnapshot: contextfrag.LifecycleSnapshot{Version: contextfrag.LifecycleSnapshotVersion},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admission := lifecycleTestAdmission()
			admitter := &agenttest.LifecycleAdmitter{Admission: admission}
			store := &agenttest.RecordingLifecycleStore{}
			service := &Service{sessionRuntime: admitter, contextLifecycles: store}

			_, subagentAdmission, terminal, err := service.AdmitSubagentRun(
				context.Background(),
				lifecycleTestBotID,
				lifecycleTestSessionID,
				"subagent:test",
				[]byte(`{"message":"work"}`),
			)
			if err != nil {
				t.Fatalf("AdmitSubagentRun() error = %v", err)
			}
			if subagentAdmission.RunID != lifecycleTestRunID {
				t.Fatalf("run ID = %q, want %q", subagentAdmission.RunID, lifecycleTestRunID)
			}
			terminal(tt.terminal())
			terminal(tools.SubagentTerminal{Cause: errors.New("late duplicate")})

			if len(store.Creates) != 1 || store.Creates[0].Status != tt.wantStatus {
				t.Fatalf("lifecycle creates = %#v, want one %s", store.Creates, tt.wantStatus)
			}
			var snapshot contextfrag.LifecycleSnapshot
			if err := json.Unmarshal(store.Creates[0].Snapshot, &snapshot); err != nil {
				t.Fatalf("decode snapshot: %v", err)
			}
			if snapshot.Version != tt.wantSnapshot.Version {
				t.Fatalf("snapshot = %#v, want version %d", snapshot, tt.wantSnapshot.Version)
			}
			if len(admitter.Finishes) != 1 || admitter.Finishes[0].Status != tt.wantFinish {
				t.Fatalf("runtime finishes = %#v, want one %s", admitter.Finishes, tt.wantFinish)
			}
		})
	}
}

func TestCanceledSubagentUsesOwningContextToPersistAborted(t *testing.T) {
	admitter := &agenttest.LifecycleAdmitter{Admission: lifecycleTestAdmission()}
	store := &agenttest.RecordingLifecycleStore{}
	service := &Service{sessionRuntime: admitter, contextLifecycles: store}
	runCtx, _, terminal, err := service.AdmitSubagentRun(
		context.Background(),
		lifecycleTestBotID,
		lifecycleTestSessionID,
		"subagent:abort",
		[]byte(`{"message":"work"}`),
	)
	if err != nil {
		t.Fatalf("AdmitSubagentRun() error = %v", err)
	}
	snapshot, ok := lifecycleTestRunConfig().ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}

	admitter.Cancel()
	<-runCtx.Done()
	terminal(tools.SubagentTerminal{Cause: context.Canceled, ContextLifecycle: &snapshot})

	if len(store.Creates) != 1 || store.Creates[0].Status != contextLifecycleStatusAborted {
		t.Fatalf("lifecycle creates = %#v, want one aborted row", store.Creates)
	}
	if len(admitter.Finishes) != 1 || admitter.Finishes[0].Status != sessionruntime.RunStatusAborted {
		t.Fatalf("runtime finishes = %#v, want one aborted finish", admitter.Finishes)
	}
}

func lifecycleTestAdmission() sessionruntime.Admission {
	return sessionruntime.Admission{
		RunID:   lifecycleTestRunID,
		Started: true,
		Handle: sessionruntime.RunHandle{
			RunID:        lifecycleTestRunID,
			BotID:        lifecycleTestBotID,
			SessionID:    lifecycleTestSessionID,
			FencingToken: 1,
		},
	}
}

func TestRecoverContextLifecycleFromAssistantMetadata(t *testing.T) {
	snapshot, ok := lifecycleTestRunConfig().ContextLifecycle.Snapshot()
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
		AssistantID: flowTestUUID(assistantMessageID),
		Metadata:    metadata,
	}
	service := &Service{contextLifecycles: store}

	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		apperror.New(apperror.CodeWorkspaceUnreachable, nil),
	)

	if len(store.Creates) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(store.Creates))
	}
	row := store.Creates[0]
	if row.Status != contextLifecycleStatusFailedProvider ||
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
	runID, botID, sessionID, err := parseContextLifecycleIDs(
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
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
			service := &Service{contextLifecycles: tt.store}
			service.recoverContextLifecycleFromAssistantMetadata(
				context.Background(),
				lifecycleTestRunID,
				lifecycleTestBotID,
				lifecycleTestSessionID,
				errors.New("continuation failed"),
			)
			if len(tt.store.Creates) != 0 || service.contextLifecyclePersistenceErrors.Load() != 0 {
				t.Fatalf("recovery writes = %d, failures = %d; want no-op", len(tt.store.Creates), service.contextLifecyclePersistenceErrors.Load())
			}
		})
	}
}

func TestRecoverContextLifecycleFromAssistantMetadataCountsStoreErrorsAndSkipsOwnershipLoss(t *testing.T) {
	metadataErr := errors.New("metadata unavailable")
	store := &agenttest.RecordingLifecycleStore{MetadataErr: metadataErr}
	service := &Service{contextLifecycles: store}
	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		errors.New("continuation failed"),
	)
	if got := service.contextLifecyclePersistenceErrors.Load(); got != 1 {
		t.Fatalf("persistence failure count = %d, want 1", got)
	}

	ownershipStore := &agenttest.RecordingLifecycleStore{}
	service = &Service{contextLifecycles: ownershipStore}
	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
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
			status:    contextLifecycleStatusFailedBudget,
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
			status:    contextLifecycleStatusFailedBudget,
			wantTrace: false,
		},
		{
			name:      "plain abort needs no trace",
			ctx:       canceledCtx,
			cause:     context.Canceled,
			status:    contextLifecycleStatusAborted,
			wantTrace: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &agenttest.RecordingLifecycleStore{}
			service := &Service{contextLifecycles: store}
			service.contextLifecycleTerminal(tt.ctx, lifecycleTestRunConfig(tt.mutations...))(tt.cause)

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
