package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/agenttest"
	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/apperror"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	"github.com/felinics/memoh/internal/runtimefence"
)

func TestContextLifecycleStatusForTerminalRunUsesDurableGenericOutcomes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		state  string
		status string
		ok     bool
	}{
		{state: "completed", status: StatusCompleted, ok: true},
		{state: "aborted", status: StatusAborted, ok: true},
		{state: "failed", status: StatusFailedProvider, ok: true},
		{state: "lost", status: StatusFailedProvider, ok: true},
		{state: "running"},
		{state: "waiting_decision"},
	} {
		t.Run(tt.state, func(t *testing.T) {
			status, ok := contextLifecycleStatusForTerminalRun(tt.state)
			if status != tt.status || ok != tt.ok {
				t.Fatalf("contextLifecycleStatusForTerminalRun(%q) = (%q, %t), want (%q, %t)", tt.state, status, ok, tt.status, tt.ok)
			}
		})
	}
}

func TestTerminalLifecycleCandidateClassificationDoesNotInheritCodeAcrossStatusChange(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	minimal := MinimalSnapshot()
	service.StageCandidate(
		lifecycleFencedContext(14),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&minimal,
		apperror.New(apperror.CodeWorkspaceUnreachable, nil),
		CandidateMinimal,
	)
	fallback := lifecycleSnapshotWithMutations(t, contextfrag.MutationRecord{
		Kind:   contextfrag.MutationContextViewFallback,
		Detail: "collector_error",
	})
	service.StageCandidate(
		lifecycleFencedContext(14),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&fallback,
		nil,
		CandidateAuthoritative,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(14, "completed", ""),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFallback || upsert.ErrorCode.Valid {
		t.Fatalf("candidate terminal = (%q, %#v), want fallback without inherited error code", upsert.Status, upsert.ErrorCode)
	}
	if !upsert.ReplaceSnapshot || upsert.ReplaceErrorCode {
		t.Fatalf("candidate authorities = snapshot:%t error_code:%t, want true/false", upsert.ReplaceSnapshot, upsert.ReplaceErrorCode)
	}
}

func TestTerminalLifecycleReconciliationPrefersCompatibleCandidateStatus(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	existingRaw, err := json.Marshal(MinimalSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{Existing: &sqlc.GetContextLifecycleByRunIDRow{
		RunID:     runUUID,
		BotID:     botUUID,
		SessionID: sessionUUID,
		Status:    StatusFailedProvider,
		ErrorCode: pgtype.Text{String: "provider.previous", Valid: true},
		Snapshot:  existingRaw,
	}}
	service := New(Deps{Store: store})
	budget := lifecycleSnapshotWithMutations(t, contextfrag.MutationRecord{
		Kind:   contextfrag.MutationContextBudgetFailure,
		Detail: "protected_context_overflow",
	})
	service.StageCandidate(
		lifecycleFencedContext(15),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&budget,
		apperror.New(apperror.CodeContextProtectedOverflow, nil),
		CandidateAuthoritative,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(15, "failed", "runtime_run_failed"),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFailedBudget ||
		!upsert.ErrorCode.Valid || upsert.ErrorCode.String != string(apperror.CodeContextProtectedOverflow) {
		t.Fatalf("candidate terminal = (%q, %#v), want failed_budget with protected-overflow code", upsert.Status, upsert.ErrorCode)
	}
	if !upsert.ReplaceSnapshot || !upsert.ReplaceErrorCode {
		t.Fatalf("candidate authorities = snapshot:%t error_code:%t, want true/true", upsert.ReplaceSnapshot, upsert.ReplaceErrorCode)
	}
}

func TestTerminalLifecycleReconciliationUsesCompatibleExistingThenDurableGeneric(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(MinimalSnapshot())
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name          string
		state         string
		runErrorCode  string
		existing      string
		existingCode  string
		wantStatus    string
		wantErrorCode string
	}{
		{
			name:       "completed accepts fallback",
			state:      "completed",
			existing:   StatusFallback,
			wantStatus: StatusFallback,
		},
		{
			name:          "failed accepts failed budget",
			state:         "failed",
			runErrorCode:  "runtime_run_failed",
			existing:      StatusFailedBudget,
			existingCode:  string(apperror.CodeContextBudgetUnsatisfied),
			wantStatus:    StatusFailedBudget,
			wantErrorCode: string(apperror.CodeContextBudgetUnsatisfied),
		},
		{
			name:          "lost accepts failed provider",
			state:         "lost",
			runErrorCode:  "runtime_run_lost",
			existing:      StatusFailedProvider,
			existingCode:  "provider.previous",
			wantStatus:    StatusFailedProvider,
			wantErrorCode: "provider.previous",
		},
		{
			name:          "lost rejects failed budget",
			state:         "lost",
			runErrorCode:  "runtime_run_lost",
			existing:      StatusFailedBudget,
			existingCode:  string(apperror.CodeContextBudgetUnsatisfied),
			wantStatus:    StatusFailedProvider,
			wantErrorCode: "runtime_run_lost",
		},
		{
			name:          "failed rejects fallback",
			state:         "failed",
			runErrorCode:  "runtime_run_failed",
			existing:      StatusFallback,
			wantStatus:    StatusFailedProvider,
			wantErrorCode: "runtime_run_failed",
		},
		{
			name:         "aborted rejects provider failure",
			state:        "aborted",
			existing:     StatusFailedProvider,
			existingCode: "provider.previous",
			wantStatus:   StatusAborted,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code := pgtype.Text{}
			if tt.existingCode != "" {
				code = pgtype.Text{String: tt.existingCode, Valid: true}
			}
			store := &agenttest.RecordingLifecycleStore{Existing: &sqlc.GetContextLifecycleByRunIDRow{
				RunID:     runUUID,
				BotID:     botUUID,
				SessionID: sessionUUID,
				Status:    tt.existing,
				ErrorCode: code,
				Snapshot:  snapshot,
			}}
			service := New(Deps{Store: store})
			service.ReconcileTerminal(
				context.Background(),
				lifecycleTerminalRun(16, tt.state, tt.runErrorCode),
			)

			if len(store.TerminalUpserts) != 1 {
				t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
			}
			upsert := store.TerminalUpserts[0]
			if upsert.Status != tt.wantStatus || upsert.ErrorCode.String != tt.wantErrorCode ||
				upsert.ErrorCode.Valid != (tt.wantErrorCode != "") {
				t.Fatalf("reconciled terminal = (%q, %#v), want (%q, %q)", upsert.Status, upsert.ErrorCode, tt.wantStatus, tt.wantErrorCode)
			}
		})
	}
}

func TestTerminalLifecycleReconciliationRejectsIncompatibleCandidateCode(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := json.Marshal(MinimalSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{Existing: &sqlc.GetContextLifecycleByRunIDRow{
		RunID:     runUUID,
		BotID:     botUUID,
		SessionID: sessionUUID,
		Status:    StatusFallback,
		Snapshot:  snapshot,
	}}
	service := New(Deps{Store: store})
	budget := lifecycleSnapshotWithMutations(t, contextfrag.MutationRecord{
		Kind: contextfrag.MutationContextBudgetFailure,
	})
	service.StageCandidate(
		lifecycleFencedContext(17),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&budget,
		apperror.New(apperror.CodeContextBudgetUnsatisfied, nil),
		CandidateAuthoritative,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(17, "completed", ""),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFallback || upsert.ErrorCode.Valid || upsert.ReplaceErrorCode {
		t.Fatalf("reconciled terminal = (%q, %#v, replace=%t), want fallback without candidate code authority", upsert.Status, upsert.ErrorCode, upsert.ReplaceErrorCode)
	}
}

func TestTerminalLifecycleCandidateWaitsForAuthoritativeFence(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	snapshot := lifecycleRichSnapshot(t)
	ctx := lifecycleFencedContext(7)

	if !service.StageCandidate(
		ctx,
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&snapshot,
		nil,
		CandidateAuthoritative,
	) {
		t.Fatal("admitted lifecycle candidate was not staged")
	}
	if len(store.TerminalUpserts) != 0 {
		t.Fatalf("candidate wrote before durable terminal: %#v", store.TerminalUpserts)
	}

	run := lifecycleTerminalRun(7, "completed", "")
	service.ReconcileTerminal(context.Background(), run)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	assertLifecycleSnapshot(t, store.TerminalUpserts[0].Snapshot, snapshot)
	if !store.TerminalUpserts[0].ReplaceSnapshot {
		t.Fatal("authoritative candidate did not replace the minimal snapshot")
	}
	if !store.TerminalUpsertBound {
		t.Fatal("terminal observer write was not bounded")
	}
	if _, ok := service.contextLifecycleCandidateFor(run); ok {
		t.Fatal("terminal candidate remained after successful upsert")
	}

	service.ReconcileTerminal(context.Background(), run)
	if len(store.TerminalUpserts) != 2 || store.TerminalUpserts[1].ReplaceSnapshot {
		t.Fatalf("duplicate terminal replay = %#v, want idempotent preserved snapshot", store.TerminalUpserts)
	}
}

func TestTerminalLifecycleCandidateOwnsErrorCodeIndependently(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	snapshot := lifecycleRichSnapshot(t)
	service.StageCandidate(
		lifecycleFencedContext(8),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&snapshot,
		apperror.New(apperror.CodeWorkspaceUnreachable, nil),
		CandidateAuthoritative,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(8, "failed", "runtime_run_failed"),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if !upsert.ReplaceSnapshot || !upsert.ReplaceErrorCode {
		t.Fatalf("candidate authorities = snapshot:%t error_code:%t, want both true", upsert.ReplaceSnapshot, upsert.ReplaceErrorCode)
	}
	if !upsert.ErrorCode.Valid || upsert.ErrorCode.String != string(apperror.CodeWorkspaceUnreachable) {
		t.Fatalf("candidate error code = %#v, want %q", upsert.ErrorCode, apperror.CodeWorkspaceUnreachable)
	}
}

func TestTerminalLifecycleMinimalCandidateOwnsOnlyErrorCode(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	snapshot := MinimalSnapshot()
	service.StageCandidate(
		lifecycleFencedContext(10),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&snapshot,
		apperror.New(apperror.CodeWorkspaceUnreachable, nil),
		CandidateMinimal,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(10, "failed", "runtime_run_failed"),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.ReplaceSnapshot || !upsert.ReplaceErrorCode {
		t.Fatalf("minimal candidate authorities = snapshot:%t error_code:%t, want false/true", upsert.ReplaceSnapshot, upsert.ReplaceErrorCode)
	}
	if !upsert.ErrorCode.Valid || upsert.ErrorCode.String != string(apperror.CodeWorkspaceUnreachable) {
		t.Fatalf("minimal candidate error code = %#v, want %q", upsert.ErrorCode, apperror.CodeWorkspaceUnreachable)
	}
}

func TestTerminalLifecycleIgnoresStaleFenceCandidate(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	snapshot := lifecycleRichSnapshot(t)
	service.StageCandidate(
		lifecycleFencedContext(3),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&snapshot,
		apperror.New(apperror.CodeWorkspaceUnreachable, nil),
		CandidateAuthoritative,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(4, "failed", "runtime_run_failed"),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	var got contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(store.TerminalUpserts[0].Snapshot, &got); err != nil {
		t.Fatal(err)
	}
	if got.Counts != (contextfrag.ManifestCounts{}) || got.AssistantMessageID != "" {
		t.Fatalf("stale candidate snapshot was used: %#v", got)
	}
	if code := store.TerminalUpserts[0].ErrorCode; !code.Valid || code.String != "runtime_run_failed" {
		t.Fatalf("terminal error code = %#v, want stable ledger code", code)
	}
	if store.TerminalUpserts[0].ReplaceErrorCode {
		t.Fatal("stale-fence candidate was granted error-code authority")
	}
	service.candidatesMu.Lock()
	remaining := len(service.candidates)
	service.candidatesMu.Unlock()
	if remaining != 0 {
		t.Fatalf("stale candidate generations remaining = %d, want 0", remaining)
	}
}

func TestTerminalLifecycleRetainsCandidateAcrossTransientStoreFailure(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{TerminalUpsertErr: errors.New("database unavailable")}
	service := New(Deps{Store: store})
	snapshot := lifecycleRichSnapshot(t)
	run := lifecycleTerminalRun(9, "completed", "")
	service.StageCandidate(
		lifecycleFencedContext(9),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&snapshot,
		nil,
		CandidateAuthoritative,
	)

	service.ReconcileTerminal(context.Background(), run)
	if _, ok := service.contextLifecycleCandidateFor(run); !ok {
		t.Fatal("candidate was discarded after a failed terminal upsert")
	}
	if got := service.PersistenceErrors(); got != 1 {
		t.Fatalf("persistence failure count = %d, want 1", got)
	}

	store.TerminalUpsertErr = nil
	service.ReconcileTerminal(context.Background(), run)
	if _, ok := service.contextLifecycleCandidateFor(run); ok {
		t.Fatal("candidate remained after retry succeeded")
	}
	if len(store.TerminalUpserts) != 2 {
		t.Fatalf("terminal upsert attempts = %d, want 2", len(store.TerminalUpserts))
	}
	assertLifecycleSnapshot(t, store.TerminalUpserts[1].Snapshot, snapshot)
}

func TestTerminalLifecycleReconcilerRepairsCrashGapWithBoundedRead(t *testing.T) {
	snapshot := lifecycleRichSnapshot(t)
	metadata, err := json.Marshal(map[string]any{contextfrag.MetadataContextLifecycleKey: snapshot})
	if err != nil {
		t.Fatal(err)
	}
	const assistantID = "44444444-4444-4444-8444-444444444444"
	store := &agenttest.RecordingLifecycleStore{
		AssistantID: agenttest.UUID(assistantID),
		Metadata:    metadata,
		TerminalRows: []sqlc.ListTerminalSessionRunsNeedingContextLifecycleRow{{
			RunID:        agenttest.UUID(agenttest.LifecycleRunID),
			BotID:        agenttest.UUID(agenttest.LifecycleBotID),
			SessionID:    agenttest.UUID(agenttest.LifecycleSessionID),
			FencingToken: 12,
			State:        "failed",
			ErrorCode:    pgtype.Text{String: "provider.stable", Valid: true},
		}},
	}
	service := New(Deps{Store: store})

	if err := service.ReconcileTerminals(context.Background()); err != nil {
		t.Fatalf("reconcileTerminalContextLifecycles() error = %v", err)
	}
	if store.TerminalListCalls != 1 || store.TerminalListLimit != contextLifecycleReconciliationBatchSize || !store.TerminalListBound {
		t.Fatalf("reconciliation list = calls:%d limit:%d bounded:%t", store.TerminalListCalls, store.TerminalListLimit, store.TerminalListBound)
	}
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFailedProvider || !upsert.ErrorCode.Valid || upsert.ErrorCode.String != "provider.stable" {
		t.Fatalf("reconciled terminal = (%q, %#v)", upsert.Status, upsert.ErrorCode)
	}
	if !upsert.ReplaceSnapshot || upsert.ReplaceErrorCode {
		t.Fatalf("recovered metadata authorities = snapshot:%t error_code:%t, want true/false", upsert.ReplaceSnapshot, upsert.ReplaceErrorCode)
	}
	var recovered contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(upsert.Snapshot, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.AssistantMessageID != assistantID {
		t.Fatalf("recovered assistant message ID = %q, want %q", recovered.AssistantMessageID, assistantID)
	}

	store.TerminalListErr = errors.New("query unavailable")
	if err := service.ReconcileTerminals(context.Background()); err == nil {
		t.Fatal("reconciliation query error was swallowed")
	}
}

func TestTerminalLifecycleReconcilerHonorsShorterDeadline(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{TerminalListWait: true}
	service := New(Deps{Store: store})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()

	err := service.ReconcileTerminals(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconciliation error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded reconciliation took %s", elapsed)
	}
	if !store.TerminalListBound {
		t.Fatal("blocking reconciliation query did not receive a deadline")
	}
}

func lifecycleRichSnapshot(t *testing.T) contextfrag.LifecycleSnapshot {
	t.Helper()
	config := agenttest.LifecycleRunConfig()
	config.ContextLifecycle.SetAssistantMessageID("55555555-5555-4555-8555-555555555555")
	snapshot, ok := config.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("lifecycle snapshot is unavailable")
	}
	return snapshot
}

func lifecycleSnapshotWithMutations(
	t *testing.T,
	mutations ...contextfrag.MutationRecord,
) contextfrag.LifecycleSnapshot {
	t.Helper()
	config := agenttest.LifecycleRunConfig(mutations...)
	snapshot, ok := config.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("lifecycle snapshot is unavailable")
	}
	return snapshot
}

func lifecycleFencedContext(token int64) context.Context {
	return runtimefence.WithContext(context.Background(), runtimefence.Fence{
		BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID, Token: token,
	})
}

func lifecycleTerminalRun(token int64, state, errorCode string) sessionruntime.TerminalRun {
	return sessionruntime.TerminalRun{
		RunID: agenttest.LifecycleRunID, BotID: agenttest.LifecycleBotID, SessionID: agenttest.LifecycleSessionID,
		FencingToken: token, State: state, ErrorCode: errorCode,
	}
}

func assertLifecycleSnapshot(t *testing.T, raw []byte, want contextfrag.LifecycleSnapshot) {
	t.Helper()
	var got contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lifecycle snapshot = %#v, want %#v", got, want)
	}
}

func TestTerminalLifecycleRepairRecoversFallbackFromAssistantMetadata(t *testing.T) {
	fallback := lifecycleSnapshotWithMutations(t, contextfrag.MutationRecord{
		Kind:   contextfrag.MutationContextViewFallback,
		Detail: "collector_error",
	})
	meta, err := json.Marshal(map[string]any{"context_lifecycle": fallback})
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{Metadata: meta}
	service := New(Deps{Store: store})

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(31, "completed", ""),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFallback || upsert.ErrorCode.Valid {
		t.Fatalf("repaired terminal = (%q, %#v), want fallback recovered from metadata", upsert.Status, upsert.ErrorCode)
	}
	if !upsert.ReplaceSnapshot {
		t.Fatal("recovered metadata snapshot should be authoritative")
	}
}

func TestTerminalLifecycleRepairRecoversFailedBudgetFromRecoveredMutations(t *testing.T) {
	budget := lifecycleSnapshotWithMutations(t, contextfrag.MutationRecord{
		Kind:   contextfrag.MutationContextBudgetFailure,
		Detail: "budget_unsatisfied",
	})
	meta, err := json.Marshal(map[string]any{"context_lifecycle": budget})
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{Metadata: meta}
	service := New(Deps{Store: store})

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(32, "failed", "runtime_run_failed"),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFailedBudget ||
		!upsert.ErrorCode.Valid || upsert.ErrorCode.String != string(apperror.CodeContextBudgetUnsatisfied) {
		t.Fatalf("repaired terminal = (%q, %#v), want failed_budget from recovered mutations", upsert.Status, upsert.ErrorCode)
	}
}

func TestTerminalLifecycleRepairClassifiesBudgetErrorCodeWithoutSnapshot(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(33, "failed", string(apperror.CodeContextBudgetUnsatisfied)),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFailedBudget ||
		!upsert.ErrorCode.Valid || upsert.ErrorCode.String != string(apperror.CodeContextBudgetUnsatisfied) {
		t.Fatalf("repaired terminal = (%q, %#v), want failed_budget from run error code", upsert.Status, upsert.ErrorCode)
	}
}

func TestTerminalLifecycleRepairKeepsRicherExistingOverMinimalCandidate(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseIDs(
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	existingRaw, err := json.Marshal(MinimalSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	store := &agenttest.RecordingLifecycleStore{Existing: &sqlc.GetContextLifecycleByRunIDRow{
		RunID:     runUUID,
		BotID:     botUUID,
		SessionID: sessionUUID,
		Status:    StatusFallback,
		Snapshot:  existingRaw,
	}}
	service := New(Deps{Store: store})
	minimal := MinimalSnapshot()
	service.StageCandidate(
		lifecycleFencedContext(34),
		agenttest.LifecycleRunID,
		agenttest.LifecycleBotID,
		agenttest.LifecycleSessionID,
		&minimal,
		nil,
		CandidateMinimal,
	)

	service.ReconcileTerminal(
		context.Background(),
		lifecycleTerminalRun(34, "completed", ""),
	)
	if len(store.TerminalUpserts) != 1 {
		t.Fatalf("terminal upserts = %d, want 1", len(store.TerminalUpserts))
	}
	upsert := store.TerminalUpserts[0]
	if upsert.Status != StatusFallback {
		t.Fatalf("repaired terminal status = %q, want richer existing fallback preserved", upsert.Status)
	}
}

func TestDiscardCandidateDropsOnlyTheNamedFence(t *testing.T) {
	store := &agenttest.RecordingLifecycleStore{}
	service := New(Deps{Store: store})
	snapshot := lifecycleRichSnapshot(t)
	for _, token := range []int64{3, 4} {
		service.StageCandidate(
			lifecycleFencedContext(token),
			agenttest.LifecycleRunID, agenttest.LifecycleBotID, agenttest.LifecycleSessionID,
			&snapshot, nil, CandidateAuthoritative,
		)
	}

	service.DiscardCandidate(agenttest.LifecycleRunID, 3)

	if _, ok := service.contextLifecycleCandidateFor(lifecycleTerminalRun(3, "completed", "")); ok {
		t.Fatal("candidate of the discarded fence is still staged")
	}
	if _, ok := service.contextLifecycleCandidateFor(lifecycleTerminalRun(4, "completed", "")); !ok {
		t.Fatal("candidate of another fence was discarded")
	}
}
