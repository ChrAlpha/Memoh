package decide

import (
	"context"
	"log/slog"
	"testing"
	"time"

	sdk "github.com/felinics/twilight/sdk"
	"github.com/google/uuid"

	"github.com/felinics/memoh/internal/agent/agenttest"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	"github.com/felinics/memoh/internal/agent/toolexec"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/runtimefence"
	"github.com/felinics/memoh/internal/sessionfacts"
)

type askWritePolicyProvider struct{}

func (askWritePolicyProvider) ToolApprovalPolicy(context.Context, string) (toolapproval.PolicyConfig, error) {
	return toolapproval.PolicyConfig{
		Enabled: true,
		Write:   toolapproval.FilePolicy{Mode: toolapproval.PolicyModeAsk, RequireApproval: true},
	}, nil
}

// A session that cannot ask anyone must leave a rejected row that names no
// deciding identity and was never pending.
func TestNonInteractiveApprovalIsRecordedAsSystemRejection(t *testing.T) {
	ctx := context.Background()
	pool := agenttest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)

	identityID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO channel_identities (id, channel_type, channel_subject_id) VALUES ($1, 'telegram', $2)`,
		identityID, "sysreject-"+identityID.String()); err != nil {
		t.Fatalf("create channel identity: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.WithoutCancel(ctx), "DELETE FROM channel_identities WHERE id = $1", identityID)
	})

	store := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	approvals := toolapproval.NewService(slog.New(slog.DiscardHandler), store, askWritePolicyProvider{})
	svc := New(Deps{Logger: slog.New(slog.DiscardHandler), ToolApproval: approvals})

	for _, mode := range []string{sessionmode.Discuss, sessionmode.Schedule, sessionmode.Subagent} {
		handler := svc.ApprovalHandler(HandlerParams{
			BotID:             botID,
			SessionID:         sessionID,
			DecisionMode:      sessionfacts.DecisionAutoReject,
			ChannelIdentityID: identityID.String(),
		})
		result, err := handler(ctx, sdk.ToolCall{
			ToolCallID: "call-" + mode,
			ToolName:   "write",
			Input:      toolexec.ArgumentsFromValue(map[string]any{"path": "/etc/x", "content": "x"}),
		})
		if err != nil {
			t.Fatalf("%s: handler error = %v", mode, err)
		}
		if result.Decision != toolexec.ToolApprovalDecisionRejected ||
			result.Reason != "tool execution requires approval, but this session type cannot request approval" {
			t.Fatalf("%s: result = %+v, want the unchanged rejection", mode, result)
		}

		var status, reason string
		var decidedBy *uuid.UUID
		var decidedAtSet bool
		if err := pool.QueryRow(ctx, `
			SELECT status, decision_reason, decided_by_channel_identity_id, decided_at IS NOT NULL
			FROM tool_approval_requests WHERE id = $1`, result.ApprovalID).Scan(&status, &reason, &decidedBy, &decidedAtSet); err != nil {
			t.Fatalf("%s: load approval row: %v", mode, err)
		}
		if status != toolapproval.StatusRejected || !decidedAtSet || decidedBy != nil {
			t.Fatalf("%s: row status=%q decided_at set=%v decided_by=%v, want rejected, decided, no deciding identity", mode, status, decidedAtSet, decidedBy)
		}
		if reason != result.Reason {
			t.Fatalf("%s: stored reason = %q, want %q", mode, reason, result.Reason)
		}
		req, err := approvals.Get(ctx, result.ApprovalID)
		if err != nil {
			t.Fatalf("%s: Get() error = %v", mode, err)
		}
		if req.DecidedByUser {
			t.Fatalf("%s: system rejection reads as a user decision", mode)
		}
	}

	var rows, pending int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status = 'pending') FROM tool_approval_requests WHERE session_id = $1`, sessionID).Scan(&rows, &pending); err != nil {
		t.Fatal(err)
	}
	if rows != 3 || pending != 0 {
		t.Fatalf("rows=%d pending=%d, want 3 rows and none pending", rows, pending)
	}
}

// A lost run leaves nobody to answer, so both its approval and its ask_user
// request close; a newer run's pending decisions stay untouched.
func TestLostRunCancelsApprovalAndAskUser(t *testing.T) {
	ctx := context.Background()
	pool := agenttest.OpenPostgres(t, ctx)
	botID, sessionID := agenttest.CreateSessionFixture(t, ctx, pool)

	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{
		OwnerID:       "owner-lost-run-test",
		StateTTL:      time.Minute,
		OwnerLeaseTTL: time.Second,
		Ledger:        ledger.NewPostgres(dbsqlc.New(pool), pool),
		Fence:         runtimefence.NewActivator(postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))),
	})
	t.Cleanup(func() { _ = manager.Close() })

	store := postgresstore.NewQueriesWithPool(pool, dbsqlc.New(pool))
	log := slog.New(slog.DiscardHandler)
	inputs := userinput.NewService(log, store)
	approvals := toolapproval.NewService(log, store, nil)
	svc := New(Deps{Logger: log, UserInput: inputs, ToolApproval: approvals})
	admission, err := manager.Admit(ctx, sessionruntime.AdmitInput{
		BotID: botID, SessionID: sessionID, InvocationID: "ext-lost", Payload: []byte(`{"query":"hi"}`),
		Execution: sessionruntime.Execution{Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
			return sessionruntime.RunAdmissionView{}, nil
		}},
	})
	if err != nil {
		t.Fatalf("Admit() error = %v", err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
		defer cancel()
		_, _ = manager.FinishRun(c, admission.Handle, sessionruntime.RunStatusCompleted)
	})
	fenced := runtimefence.WithContext(ctx, runtimefence.Fence{BotID: botID, SessionID: sessionID, Token: admission.Handle.FencingToken})

	approval, err := approvals.CreatePending(fenced, toolapproval.CreatePendingInput{
		BotID: botID, SessionID: sessionID, ToolCallID: "w1", ToolName: "write",
		ToolInput: map[string]any{"path": "/etc/x"},
	})
	if err != nil {
		t.Fatalf("CreatePending approval: %v", err)
	}
	ask, err := inputs.CreatePending(fenced, userinput.CreatePendingInput{
		BotID: botID, SessionID: sessionID, ToolCallID: "a1", ToolName: userinput.ToolNameAskUser,
		Input: map[string]any{"questions": []any{map[string]any{
			"text": "Choose", "kind": userinput.QuestionKindSingleSelect,
			"options": []any{map[string]any{"label": "A"}, map[string]any{"label": "B"}},
		}}},
	})
	if err != nil {
		t.Fatalf("CreatePending ask_user: %v", err)
	}

	if err := svc.CancelLostRunDecisions(ctx, botID, sessionID, admission.Handle.RunID, admission.Handle.FencingToken, "run_lost"); err != nil {
		t.Fatalf("cancelLostRunDecisions() error = %v", err)
	}

	var approvalStatus, approvalReason, askStatus string
	var decidedBy *uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT status, decision_reason, decided_by_channel_identity_id FROM tool_approval_requests WHERE id = $1`, approval.ID).Scan(&approvalStatus, &approvalReason, &decidedBy); err != nil {
		t.Fatal(err)
	}
	if approvalStatus != toolapproval.StatusCancelled || approvalReason != "run_lost" || decidedBy != nil {
		t.Fatalf("approval status=%q reason=%q decided_by=%v, want cancelled by the system for run_lost", approvalStatus, approvalReason, decidedBy)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM user_input_requests WHERE id = $1`, ask.ID).Scan(&askStatus); err != nil {
		t.Fatal(err)
	}
	if askStatus != "canceled" {
		t.Fatalf("ask_user status = %q, want canceled", askStatus)
	}
}
