package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	contextfrag "github.com/memohai/memoh/internal/agent/context/fragment"
	"github.com/memohai/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/memohai/memoh/internal/agent/runtime/session"
	tools "github.com/memohai/memoh/internal/agent/tool"
	"github.com/memohai/memoh/internal/apperror"
	"github.com/memohai/memoh/internal/db"
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
	params          []sqlc.CreateContextLifecycleParams
	err             error
	existing        *sqlc.ContextLifecycle
	existingAfter   int
	getCalls        int
	getErr          error
	metadata        []byte
	metadataErr     error
	pendingApproval *sqlc.ToolApprovalRequest
	pendingInput    *sqlc.UserInputRequest
	pendingUntil    int
	pendingReads    int
	sessionRun      sqlc.SessionRun
	sessionRunErr   error
	upsertMu        sync.Mutex
	upsertParams    []sqlc.UpsertAbortedContextLifecycleParams
	upsertErr       error
	upsertCh        chan struct{}
	updateParams    []sqlc.UpdateAbortedContextLifecycleSnapshotParams
	updateErr       error
}

func (q *recordingContextLifecycleQueries) CreateContextLifecycle(
	_ context.Context,
	arg sqlc.CreateContextLifecycleParams,
) (sqlc.ContextLifecycle, error) {
	q.params = append(q.params, arg)
	if q.err != nil {
		return sqlc.ContextLifecycle{}, q.err
	}
	created := sqlc.ContextLifecycle{
		RunID:     arg.RunID,
		BotID:     arg.BotID,
		SessionID: arg.SessionID,
		Status:    arg.Status,
		ErrorCode: arg.ErrorCode,
		Snapshot:  append([]byte(nil), arg.Snapshot...),
	}
	if q.existing == nil {
		q.existing = &created
	}
	return created, nil
}

func (q *recordingContextLifecycleQueries) GetContextLifecycleByRunID(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.ContextLifecycle, error) {
	q.getCalls++
	if q.getErr != nil {
		return sqlc.ContextLifecycle{}, q.getErr
	}
	if q.existing == nil || q.existingAfter > 0 && q.getCalls < q.existingAfter {
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

func (q *recordingContextLifecycleQueries) GetSessionRun(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.SessionRun, error) {
	if q.sessionRunErr != nil {
		return sqlc.SessionRun{}, q.sessionRunErr
	}
	return q.sessionRun, nil
}

func (q *recordingContextLifecycleQueries) GetPendingToolApprovalByRun(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.ToolApprovalRequest, error) {
	q.pendingReads++
	if q.pendingUntil > 0 && q.pendingReads > q.pendingUntil {
		return sqlc.ToolApprovalRequest{}, pgx.ErrNoRows
	}
	if q.pendingApproval == nil {
		return sqlc.ToolApprovalRequest{}, pgx.ErrNoRows
	}
	return *q.pendingApproval, nil
}

func (q *recordingContextLifecycleQueries) GetPendingUserInputByRun(
	_ context.Context,
	_ pgtype.UUID,
) (sqlc.UserInputRequest, error) {
	if q.pendingInput == nil {
		return sqlc.UserInputRequest{}, pgx.ErrNoRows
	}
	return *q.pendingInput, nil
}

func (q *recordingContextLifecycleQueries) UpsertAbortedContextLifecycle(
	_ context.Context,
	arg sqlc.UpsertAbortedContextLifecycleParams,
) (sqlc.ContextLifecycle, error) {
	q.upsertMu.Lock()
	q.upsertParams = append(q.upsertParams, arg)
	q.upsertMu.Unlock()
	if q.upsertCh != nil {
		select {
		case q.upsertCh <- struct{}{}:
		default:
		}
	}
	return sqlc.ContextLifecycle{}, q.upsertErr
}

func (q *recordingContextLifecycleQueries) UpdateAbortedContextLifecycleSnapshot(
	_ context.Context,
	arg sqlc.UpdateAbortedContextLifecycleSnapshotParams,
) (sqlc.ContextLifecycle, error) {
	q.updateParams = append(q.updateParams, arg)
	if q.updateErr != nil {
		return sqlc.ContextLifecycle{}, q.updateErr
	}
	if q.existing == nil || q.existing.Status != contextLifecycleStatusAborted {
		return sqlc.ContextLifecycle{}, pgx.ErrNoRows
	}
	updated := *q.existing
	updated.Snapshot = append([]byte(nil), arg.Snapshot...)
	q.existing = &updated
	return updated, nil
}

func (q *recordingContextLifecycleQueries) abortedUpserts() []sqlc.UpsertAbortedContextLifecycleParams {
	q.upsertMu.Lock()
	defer q.upsertMu.Unlock()
	return append([]sqlc.UpsertAbortedContextLifecycleParams(nil), q.upsertParams...)
}

type recordingAbortRuntime struct {
	applied bool
	err     error
}

func (r *recordingAbortRuntime) AbortControl(
	context.Context,
	string,
	string,
	string,
	string,
) (bool, error) {
	return r.applied, r.err
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
		{
			name:   "aborted",
			cause:  context.Canceled,
			status: contextLifecycleStatusAborted,
		},
		{
			name: "budget failure outranks cancellation",
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

func TestSubagentFailureBeforeContextAssemblyPersistsMinimalLifecycle(t *testing.T) {
	const admittedRunID = "55555555-5555-4555-8555-555555555556"
	runtime := &lifecycleSubagentRuntime{runID: admittedRunID}
	queries := &recordingContextLifecycleQueries{}
	service := &Service{
		sessionRuntime:    runtime,
		contextLifecycles: queries,
	}
	_, runID, terminal, err := service.AdmitSubagentRun(
		context.Background(),
		lifecycleTestBotID,
		lifecycleTestSessionID,
		"subagent:pre-context-failure",
		[]byte(`{"message":"work"}`),
	)
	if err != nil {
		t.Fatalf("AdmitSubagentRun error: %v", err)
	}
	runErr := errors.New("resolve subagent model")
	terminal(tools.SubagentTerminal{Cause: runErr})

	if runID != admittedRunID {
		t.Fatalf("run ID = %q, want admitted RunID %q", runID, admittedRunID)
	}
	if len(queries.params) != 1 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(queries.params))
	}
	row := queries.params[0]
	if got := pgUUIDString(row.RunID); got != admittedRunID {
		t.Fatalf("persisted RunID = %q, want %q", got, admittedRunID)
	}
	if row.Status != contextLifecycleStatusFailedProvider {
		t.Fatalf("status = %q, want %q", row.Status, contextLifecycleStatusFailedProvider)
	}
	wantCode := string(apperror.CodeOf(runErr))
	if row.ErrorCode.String != wantCode || row.ErrorCode.Valid != (wantCode != "") {
		t.Fatalf("error code = %#v, want %q", row.ErrorCode, wantCode)
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(row.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode lifecycle snapshot: %v", err)
	}
	if snapshot.Version != 1 || len(snapshot.Breakdown) != 0 || len(snapshot.Mutations) != 0 {
		t.Fatalf("minimal lifecycle snapshot = %#v", snapshot)
	}
	if len(runtime.finishes) != 1 || runtime.finishes[0].status != sessionruntime.RunStatusErrored {
		t.Fatalf("runtime finishes = %#v, want one errored finish", runtime.finishes)
	}
}

func TestRuntimeDecisionTerminalWithoutRecoverableSnapshotPersistsMinimalLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		cause      error
		wantStatus string
	}{
		{
			name:       "completed continuation",
			wantStatus: contextLifecycleStatusCompleted,
		},
		{
			name:       "failed continuation",
			cause:      errors.New("resume failed before context assembly"),
			wantStatus: contextLifecycleStatusFailedProvider,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := sessionruntime.NewManager(
				sessionruntime.NewMemoryBackend(),
				sessionruntime.Options{
					OwnerID:       "round5-decision-owner",
					StateTTL:      time.Minute,
					OwnerLeaseTTL: time.Second,
					CommandAckTTL: time.Second,
				},
			)
			t.Cleanup(func() { _ = manager.Close() })
			if err := manager.Start(context.Background()); err != nil {
				t.Fatalf("start session runtime: %v", err)
			}
			if err := manager.StartRun(
				context.Background(),
				lifecycleTestBotID,
				lifecycleTestSessionID,
				lifecycleTestRunID,
				make(chan struct{}, 1),
				func() {},
				nil,
			); err != nil {
				t.Fatalf("start runtime run: %v", err)
			}
			runtimeSnapshot, err := manager.Snapshot(
				context.Background(),
				lifecycleTestBotID,
				lifecycleTestSessionID,
			)
			if err != nil || runtimeSnapshot.CurrentRunView == nil {
				t.Fatalf("load runtime run: %#v, %v", runtimeSnapshot.CurrentRunView, err)
			}
			lifecycles := &recordingContextLifecycleQueries{}
			service := &Service{
				decisionRuntime:   manager,
				contextLifecycles: lifecycles,
			}

			service.finishRuntimeDecision(
				context.Background(),
				sessionruntime.RunHandle{
					BotID:      lifecycleTestBotID,
					SessionID:  lifecycleTestSessionID,
					RunID:      lifecycleTestRunID,
					Generation: runtimeSnapshot.CurrentRunView.Generation,
				},
				tt.cause,
			)

			if len(lifecycles.params) != 1 {
				t.Fatalf("CreateContextLifecycle calls = %d, want 1", len(lifecycles.params))
			}
			row := lifecycles.params[0]
			if got := pgUUIDString(row.RunID); got != lifecycleTestRunID {
				t.Fatalf("context lifecycle run ID = %q, want %q", got, lifecycleTestRunID)
			}
			if row.Status != tt.wantStatus {
				t.Fatalf("context lifecycle status = %q, want %q", row.Status, tt.wantStatus)
			}
			if !bytes.Equal(row.Snapshot, mustMarshalMinimalLifecycle(t)) {
				t.Fatalf("runtime decision fallback snapshot = %s, want minimal content-light snapshot", row.Snapshot)
			}
		})
	}
}

func mustMarshalMinimalLifecycle(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(minimalContextLifecycleSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	return raw
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
	if got := queries.params[0].Status; got != contextLifecycleStatusAborted {
		t.Fatalf("lifecycle status = %q, want %q", got, contextLifecycleStatusAborted)
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

func TestAbortRuntimeRunReconcilesDurableAbortedLifecycle(t *testing.T) {
	cfg := lifecycleTestRunConfig(t, lifecycleTestRunID)
	snapshot, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	metadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: snapshot,
	})
	if err != nil {
		t.Fatalf("marshal lifecycle metadata: %v", err)
	}
	expectedSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal lifecycle snapshot: %v", err)
	}
	runUUID, err := db.ParseUUID(lifecycleTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	botUUID, err := db.ParseUUID(lifecycleTestBotID)
	if err != nil {
		t.Fatal(err)
	}
	sessionUUID, err := db.ParseUUID(lifecycleTestSessionID)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name             string
		upsertErr        error
		wantFailureCount uint64
	}{
		{name: "writes aborted lifecycle"},
		{name: "store error does not change abort acknowledgement", upsertErr: errors.New("database unavailable"), wantFailureCount: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := &recordingContextLifecycleQueries{
				metadata:        metadata,
				pendingApproval: &sqlc.ToolApprovalRequest{},
				sessionRun: sqlc.SessionRun{
					RunID:     runUUID,
					BotID:     botUUID,
					SessionID: sessionUUID,
					State:     "aborted",
				},
				upsertErr: tt.upsertErr,
				upsertCh:  make(chan struct{}, 1),
			}
			service := &Service{
				queries:           queries,
				contextLifecycles: queries,
				abortRuntime:      &recordingAbortRuntime{applied: true},
			}

			applied, err := service.AbortRuntimeRun(
				context.Background(),
				lifecycleTestBotID,
				lifecycleTestSessionID,
				lifecycleTestRunID,
				"abort-control-1",
			)
			if err != nil || !applied {
				t.Fatalf("AbortRuntimeRun() = (%t, %v), want (true, nil)", applied, err)
			}
			select {
			case <-queries.upsertCh:
			case <-time.After(time.Second):
				t.Fatal("aborted lifecycle reconciliation did not run")
			}
			upserts := queries.abortedUpserts()
			if len(upserts) != 1 {
				t.Fatalf("aborted lifecycle upserts = %d, want 1", len(upserts))
			}
			if got := pgUUIDString(upserts[0].RunID); got != lifecycleTestRunID {
				t.Fatalf("aborted lifecycle run ID = %q, want %q", got, lifecycleTestRunID)
			}
			if !bytes.Equal(upserts[0].Snapshot, expectedSnapshot) {
				t.Fatalf("reconciled snapshot = %s, want assistant metadata %s", upserts[0].Snapshot, expectedSnapshot)
			}
			deadline := time.Now().Add(time.Second)
			for service.contextLifecyclePersistenceErrors.Load() != tt.wantFailureCount && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := service.contextLifecyclePersistenceErrors.Load(); got != tt.wantFailureCount {
				t.Fatalf("persistence error count = %d, want %d", got, tt.wantFailureCount)
			}
		})
	}
}

func TestAbortRuntimeRunWithoutContextPersistsMinimalLifecycle(t *testing.T) {
	runUUID, botUUID, sessionUUID, err := parseContextLifecycleIDs(
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	queries := &recordingContextLifecycleQueries{
		sessionRun: sqlc.SessionRun{
			RunID:     runUUID,
			BotID:     botUUID,
			SessionID: sessionUUID,
			State:     "aborted",
		},
		upsertCh: make(chan struct{}, 1),
	}
	service := &Service{
		queries:           queries,
		contextLifecycles: queries,
		abortRuntime:      &recordingAbortRuntime{applied: true},
	}

	applied, err := service.AbortRuntimeRun(
		context.Background(),
		lifecycleTestBotID,
		lifecycleTestSessionID,
		lifecycleTestRunID,
		"abort-before-context",
	)
	if err != nil || !applied {
		t.Fatalf("AbortRuntimeRun() = (%t, %v), want (true, nil)", applied, err)
	}
	if len(queries.params) != 0 {
		t.Fatalf("CreateContextLifecycle calls = %d, want 0 before durable abort reconciliation", len(queries.params))
	}
	select {
	case <-queries.upsertCh:
	case <-time.After(2 * time.Second):
		t.Fatal("minimal aborted lifecycle was not reconciled")
	}
	upserts := queries.abortedUpserts()
	if len(upserts) != 1 {
		t.Fatalf("aborted lifecycle upserts = %d, want 1", len(upserts))
	}
	var snapshot contextfrag.LifecycleSnapshot
	if err := json.Unmarshal(upserts[0].Snapshot, &snapshot); err != nil {
		t.Fatalf("decode minimal snapshot: %v", err)
	}
	minimalRaw, err := json.Marshal(minimalContextLifecycleSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(upserts[0].Snapshot, minimalRaw) || snapshot.Version != 1 {
		t.Fatalf("early abort snapshot = %#v, want content-light minimal version 1", snapshot)
	}
}

func TestAbortReconciliationPrefersResumedRunSnapshotOverPausedMetadata(t *testing.T) {
	runUUID, err := db.ParseUUID(lifecycleTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	botUUID, err := db.ParseUUID(lifecycleTestBotID)
	if err != nil {
		t.Fatal(err)
	}
	sessionUUID, err := db.ParseUUID(lifecycleTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	staleMetadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: contextfrag.LifecycleSnapshot{Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalSnapshot, err := json.Marshal(contextfrag.LifecycleSnapshot{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	queries := &recordingContextLifecycleQueries{
		existing:      &sqlc.ContextLifecycle{Snapshot: finalSnapshot},
		existingAfter: 2,
		metadata:      staleMetadata,
		sessionRun: sqlc.SessionRun{
			RunID:     runUUID,
			BotID:     botUUID,
			SessionID: sessionUUID,
			State:     "aborted",
		},
		upsertCh: make(chan struct{}, 1),
	}
	service := &Service{
		queries:           queries,
		contextLifecycles: queries,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	service.reconcileAbortedContextLifecycle(
		ctx,
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)

	upserts := queries.abortedUpserts()
	if len(upserts) != 1 {
		t.Fatalf("aborted lifecycle upserts = %d, want 1", len(upserts))
	}
	if !bytes.Equal(upserts[0].Snapshot, finalSnapshot) {
		t.Fatalf("aborted lifecycle snapshot = %s, want resumed snapshot %s", upserts[0].Snapshot, finalSnapshot)
	}
	if bytes.Contains(upserts[0].Snapshot, []byte(`"version":1`)) {
		t.Fatalf("aborted lifecycle used stale paused metadata: %s", upserts[0].Snapshot)
	}
}

func TestAbortReconciliationRechecksPendingDecisionBeforeMetadataFallback(t *testing.T) {
	runUUID, err := db.ParseUUID(lifecycleTestRunID)
	if err != nil {
		t.Fatal(err)
	}
	botUUID, err := db.ParseUUID(lifecycleTestBotID)
	if err != nil {
		t.Fatal(err)
	}
	sessionUUID, err := db.ParseUUID(lifecycleTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	staleMetadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: contextfrag.LifecycleSnapshot{Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalSnapshot, err := json.Marshal(contextfrag.LifecycleSnapshot{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	queries := &recordingContextLifecycleQueries{
		existing:        &sqlc.ContextLifecycle{Snapshot: finalSnapshot},
		existingAfter:   4,
		metadata:        staleMetadata,
		pendingApproval: &sqlc.ToolApprovalRequest{},
		pendingUntil:    1,
		sessionRun: sqlc.SessionRun{
			RunID:     runUUID,
			BotID:     botUUID,
			SessionID: sessionUUID,
			State:     "aborted",
		},
	}
	service := &Service{
		queries:           queries,
		contextLifecycles: queries,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	service.reconcileAbortedContextLifecycle(
		ctx,
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)

	upserts := queries.abortedUpserts()
	if len(upserts) != 1 {
		t.Fatalf("aborted lifecycle upserts = %d, want 1", len(upserts))
	}
	if !bytes.Equal(upserts[0].Snapshot, finalSnapshot) {
		t.Fatalf("aborted lifecycle snapshot = %s, want resumed snapshot %s", upserts[0].Snapshot, finalSnapshot)
	}
	if elapsedReads := queries.pendingReads; elapsedReads < 2 {
		t.Fatalf("pending decision reads = %d, want a recheck", elapsedReads)
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

func TestAuthoritativeContextLifecycleReplacesRecoveredAbortedSnapshot(t *testing.T) {
	runID, botID, sessionID, err := parseContextLifecycleIDs(
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	queries := &recordingContextLifecycleQueries{
		err: &pgconn.PgError{Code: "23505", ConstraintName: "context_lifecycles_pkey"},
		existing: &sqlc.ContextLifecycle{
			RunID:     runID,
			BotID:     botID,
			SessionID: sessionID,
			Status:    contextLifecycleStatusAborted,
			Snapshot:  []byte(`{"version":1}`),
		},
	}
	service := &Service{contextLifecycles: queries}

	service.contextLifecycleTerminal(
		context.Background(),
		lifecycleTestRunConfig(t, lifecycleTestRunID),
	)(nil)

	if got := service.contextLifecyclePersistenceErrors.Load(); got != 0 {
		t.Fatalf("persistence error count = %d, want 0 for converged aborted row", got)
	}
	if len(queries.updateParams) != 1 {
		t.Fatalf("aborted snapshot updates = %d, want 1", len(queries.updateParams))
	}
	if bytes.Equal(queries.updateParams[0].Snapshot, []byte(`{"version":1}`)) {
		t.Fatalf("authoritative terminal kept recovered paused snapshot: %s", queries.updateParams[0].Snapshot)
	}
	if queries.existing.Status != contextLifecycleStatusAborted || queries.existing.ErrorCode.Valid {
		t.Fatalf("converged lifecycle terminal = (%q, %#v), want aborted with no error code", queries.existing.Status, queries.existing.ErrorCode)
	}
}

func TestRecoveredMetadataCannotReplaceAuthoritativeAbortedSnapshot(t *testing.T) {
	cfg := lifecycleTestRunConfig(t, lifecycleTestRunID)
	paused, ok := cfg.ContextLifecycle.Snapshot()
	if !ok {
		t.Fatal("test lifecycle snapshot is unavailable")
	}
	paused.Version = 1
	metadata, err := json.Marshal(map[string]any{
		contextfrag.MetadataContextLifecycleKey: paused,
	})
	if err != nil {
		t.Fatal(err)
	}
	runID, botID, sessionID, err := parseContextLifecycleIDs(
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := []byte(`{"version":2}`)
	queries := &recordingContextLifecycleQueries{
		existing: &sqlc.ContextLifecycle{
			RunID:     runID,
			BotID:     botID,
			SessionID: sessionID,
			Status:    contextLifecycleStatusAborted,
			Snapshot:  authoritative,
		},
		metadata: metadata,
	}
	service := &Service{contextLifecycles: queries}

	service.recoverContextLifecycleFromAssistantMetadata(
		context.Background(),
		lifecycleTestRunID,
		lifecycleTestBotID,
		lifecycleTestSessionID,
		errors.New("late recovery"),
	)

	if len(queries.params) != 0 || len(queries.updateParams) != 0 {
		t.Fatalf("metadata recovery wrote over an existing authoritative snapshot: creates=%d updates=%d", len(queries.params), len(queries.updateParams))
	}
	if !bytes.Equal(queries.existing.Snapshot, authoritative) {
		t.Fatalf("authoritative snapshot = %s, want %s", queries.existing.Snapshot, authoritative)
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
