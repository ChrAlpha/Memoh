package application

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/runtime/session/ledger"
	"github.com/felinics/memoh/internal/bots"
	"github.com/felinics/memoh/internal/db"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
	postgresstore "github.com/felinics/memoh/internal/db/postgres/store"
	"github.com/felinics/memoh/internal/runtimefence"
)

// grantPermissionChecker resolves bot permissions from the real grant rows,
// the way the server's checker does for a non-admin account.
type grantPermissionChecker struct {
	bots *bots.Service
}

func (c grantPermissionChecker) HasBotPermission(ctx context.Context, botID, accountID, permission string) (bool, error) {
	perms, err := c.bots.ResolveUserPermissions(ctx, botID, accountID, false)
	if err != nil {
		return false, err
	}
	return bots.HasPermission(perms, permission), nil
}

type approvalAuthorityFixture struct {
	botID       string
	sessionID   string
	legacyID    string
	initiator   string
	manager     string
	grantHolder string
	outsider    string
}

// createApprovalAuthorityFixture seeds one bot whose owner takes no part, and
// four members: the requester and a bystander both hold workspace_exec, one
// member manages the bot, and one may only chat.
func createApprovalAuthorityFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) approvalAuthorityFixture {
	t.Helper()
	ownerID := uuid.New()
	members := map[string]uuid.UUID{
		"initiator":    uuid.New(),
		"manager":      uuid.New(),
		"grant-holder": uuid.New(),
		"outsider":     uuid.New(),
	}
	grants := map[string]string{
		"initiator":    `["chat","workspace_exec"]`,
		"manager":      `["manage"]`,
		"grant-holder": `["chat","workspace_exec"]`,
		"outsider":     `["chat"]`,
	}
	botID, sessionID, legacyID := uuid.New(), uuid.New(), uuid.New()
	cleanupCtx := context.WithoutCancel(ctx)
	users := []uuid.UUID{ownerID}
	for _, id := range members {
		users = append(users, id)
	}
	for _, id := range users {
		if _, err := pool.Exec(ctx, `
			WITH created_user AS (
				INSERT INTO users (id, username, is_active) VALUES ($1, $2, true) RETURNING id
			)
			INSERT INTO team_members (user_id, role) SELECT id, 'member' FROM created_user
		`, id, "approval-authority-"+id.String()); err != nil {
			t.Fatalf("create user fixture: %v", err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(cleanupCtx, "DELETE FROM users WHERE id = $1", id) })
	}
	if _, err := pool.Exec(ctx, `INSERT INTO bots (id, owner_user_id, name) VALUES ($1, $2, $3)`, botID, ownerID, "approval-authority-"+botID.String()); err != nil {
		t.Fatalf("create bot fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(cleanupCtx, "DELETE FROM bots WHERE id = $1", botID) })
	for role, id := range members {
		if _, err := pool.Exec(ctx, `
			INSERT INTO bot_user_grants (bot_id, subject_type, user_id, permissions) VALUES ($1, 'user', $2, $3::jsonb)
		`, botID, id, grants[role]); err != nil {
			t.Fatalf("create %s grant: %v", role, err)
		}
	}
	for _, id := range []uuid.UUID{sessionID, legacyID} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO bot_sessions (id, bot_id, channel_type, runtime_type) VALUES ($1, $2, 'telegram', 'model')
		`, id, botID); err != nil {
			t.Fatalf("create session fixture: %v", err)
		}
	}
	return approvalAuthorityFixture{
		botID:       botID.String(),
		sessionID:   sessionID.String(),
		legacyID:    legacyID.String(),
		initiator:   members["initiator"].String(),
		manager:     members["manager"].String(),
		grantHolder: members["grant-holder"].String(),
		outsider:    members["outsider"].String(),
	}
}

// The whole chain on PostgreSQL: admission records the requester, the
// approval inherits the run, and both the per-viewer flag and the submitted
// decision are answered by CanDecideToolApproval against real grant rows.
func TestPostgresToolApprovalAuthority(t *testing.T) {
	ctx := t.Context()
	pool := openTurnAdmissionPostgres(t, ctx)
	fx := createApprovalAuthorityFixture(t, ctx, pool)
	q := sqlc.New(pool)
	store := postgresstore.NewQueriesWithPool(pool, q)
	runs := ledger.NewPostgres(q, pool)
	manager := sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{Ledger: runs, Fence: runtimefence.NewActivator(store)})
	t.Cleanup(func() { _ = manager.CloseContext(context.Background()) })
	approvals := toolapproval.NewService(nil, store, nil)
	svc := &Service{
		queries:        store,
		toolApproval:   approvals,
		botPermissions: grantPermissionChecker{bots: bots.NewService(slog.Default(), store)},
	}

	// park admits a run in sessionID under initiatorID and parks it on n
	// pending exec approvals, returning the owner's context and the approvals.
	park := func(sessionID, initiatorID string, n int) (context.Context, []toolapproval.Request) {
		t.Helper()
		admission, err := manager.Admit(ctx, sessionruntime.AdmitInput{
			BotID: fx.botID, SessionID: sessionID, InvocationID: "approval-authority-" + uuid.NewString(),
			InitiatorID: initiatorID, Payload: []byte(`{"query":"run it"}`),
			Execution: sessionruntime.Execution{Admission: func(context.Context, sessionruntime.RunHandle) (sessionruntime.RunAdmissionView, error) {
				return sessionruntime.RunAdmissionView{}, nil
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		row, err := q.GetSessionRun(ctx, db.ParseUUIDOrEmpty(admission.RunID))
		if err != nil {
			t.Fatal(err)
		}
		if got := db.TextToString(row.InitiatorID); got != initiatorID || row.InitiatorID.Valid != (initiatorID != "") {
			t.Fatalf("session_runs.initiator_id = %+v, want %q", row.InitiatorID, initiatorID)
		}
		ownerCtx := runtimefence.WithContext(ctx, runtimefence.Fence{BotID: fx.botID, SessionID: sessionID, Token: admission.Handle.FencingToken})
		created := make([]toolapproval.Request, 0, n)
		for i := range n {
			approval, err := approvals.CreatePending(ownerCtx, toolapproval.CreatePendingInput{
				BotID: fx.botID, SessionID: sessionID, ToolCallID: "exec-" + uuid.NewString(), ToolName: "exec",
				ToolInput: map[string]any{"command": "make deploy", "n": i},
			})
			if err != nil {
				t.Fatal(err)
			}
			if approval.RunID != admission.RunID || approval.Operation != toolapproval.OperationExec {
				t.Fatalf("approval run/operation = %q/%q, want %q/exec", approval.RunID, approval.Operation, admission.RunID)
			}
			created = append(created, approval)
		}
		if _, _, err := runs.SetWaitingDecision(ctx, admission.RunID, admission.Handle.FencingToken); err != nil {
			t.Fatal(err)
		}
		return ownerCtx, created
	}
	viewerCanDecide := func(approvalID, actor string) bool {
		t.Helper()
		ok, err := svc.ViewerCanDecideToolApproval(ctx, approvalID, ToolApprovalPrincipal{UserID: actor, ChannelIdentityID: actor})
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	submit := func(ownerCtx context.Context, sessionID, approvalID, actor string) error {
		t.Helper()
		_, err := svc.CommitToolApprovalResponse(ownerCtx, ToolApprovalResponseInput{
			BotID: fx.botID, ThreadID: sessionID, ApprovalID: approvalID, Decision: "approve",
			ActorUserID: actor, ActorChannelIdentityID: actor,
		})
		return err
	}
	status := func(approvalID string) string {
		t.Helper()
		approval, err := approvals.Get(ctx, approvalID)
		if err != nil {
			t.Fatal(err)
		}
		return approval.Status
	}

	t.Run("run with a recorded initiator", func(t *testing.T) {
		ownerCtx, pending := park(fx.sessionID, fx.initiator, 2)
		first, second := pending[0], pending[1]
		for actor, want := range map[string]bool{fx.initiator: true, fx.manager: true, fx.grantHolder: false, fx.outsider: false} {
			if got := viewerCanDecide(first.ID, actor); got != want {
				t.Fatalf("viewer_can_decide for %s = %t, want %t", actor, got, want)
			}
		}
		for _, actor := range []string{fx.grantHolder, fx.outsider} {
			if err := submit(ownerCtx, fx.sessionID, first.ID, actor); !errors.Is(err, toolapproval.ErrForbidden) {
				t.Fatalf("decision by %s error = %v, want forbidden", actor, err)
			}
		}
		if got := status(first.ID); got != toolapproval.StatusPending {
			t.Fatalf("refused decisions changed the approval to %q", got)
		}
		if err := submit(ownerCtx, fx.sessionID, first.ID, fx.initiator); err != nil {
			t.Fatalf("initiator decision: %v", err)
		}
		if err := submit(ownerCtx, fx.sessionID, second.ID, fx.manager); err != nil {
			t.Fatalf("manager decision: %v", err)
		}
		if status(first.ID) != toolapproval.StatusApproved || status(second.ID) != toolapproval.StatusApproved {
			t.Fatal("allowed decisions were not committed")
		}
	})

	t.Run("run without a recorded initiator", func(t *testing.T) {
		ownerCtx, pending := park(fx.legacyID, "", 1)
		approval := pending[0]
		// Whoever actually sent the request is not on record, so only the
		// bot's manager may decide.
		if viewerCanDecide(approval.ID, fx.initiator) || !viewerCanDecide(approval.ID, fx.manager) {
			t.Fatal("run without initiator offered the decision to the wrong viewers")
		}
		if err := submit(ownerCtx, fx.legacyID, approval.ID, fx.initiator); !errors.Is(err, toolapproval.ErrForbidden) {
			t.Fatalf("unrecorded requester decision error = %v, want forbidden", err)
		}
		if err := submit(ownerCtx, fx.legacyID, approval.ID, fx.manager); err != nil {
			t.Fatalf("manager decision: %v", err)
		}
	})
}
