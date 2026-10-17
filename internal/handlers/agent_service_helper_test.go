package handlers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent/application"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
	dbsqlc "github.com/felinics/memoh/internal/db/postgres/sqlc"
	dbstore "github.com/felinics/memoh/internal/db/store"
	"github.com/felinics/memoh/internal/models"
	"github.com/felinics/memoh/internal/settings"
	"github.com/felinics/memoh/internal/workspace"
)

// stubAgentQueries answers the reads a run's end makes with empty results; any
// other query panics, which names the test that needs a real store.
type stubAgentQueries struct{ dbstore.Queries }

func (stubAgentQueries) ListToolApprovalsByRun(context.Context, pgtype.UUID) ([]dbsqlc.ToolApprovalRequest, error) {
	return nil, nil
}

func (stubAgentQueries) GetContextLifecycleByRunID(context.Context, pgtype.UUID) (dbsqlc.GetContextLifecycleByRunIDRow, error) {
	return dbsqlc.GetContextLifecycleByRunIDRow{}, pgx.ErrNoRows
}

func (stubAgentQueries) UpsertTerminalContextLifecycle(context.Context, dbsqlc.UpsertTerminalContextLifecycleParams) (dbsqlc.UpsertTerminalContextLifecycleRow, error) {
	return dbsqlc.UpsertTerminalContextLifecycleRow{}, nil
}

func (stubAgentQueries) CreateContextLifecycle(context.Context, dbsqlc.CreateContextLifecycleParams) (dbsqlc.CreateContextLifecycleRow, error) {
	return dbsqlc.CreateContextLifecycleRow{}, nil
}

func (stubAgentQueries) GetLatestAssistantContextLifecycleMetadataByRunID(context.Context, pgtype.UUID) ([]byte, error) {
	return nil, pgx.ErrNoRows
}

func (stubAgentQueries) UpdateAbortedContextLifecycleSnapshot(context.Context, dbsqlc.UpdateAbortedContextLifecycleSnapshotParams) (dbsqlc.UpdateAbortedContextLifecycleSnapshotRow, error) {
	return dbsqlc.UpdateAbortedContextLifecycleSnapshotRow{}, nil
}

func (stubAgentQueries) UpsertAbortedContextLifecycle(context.Context, dbsqlc.UpsertAbortedContextLifecycleParams) (dbsqlc.UpsertAbortedContextLifecycleRow, error) {
	return dbsqlc.UpsertAbortedContextLifecycleRow{}, nil
}

func (stubAgentQueries) ListTerminalSessionRunsNeedingContextLifecycle(context.Context, int32) ([]dbsqlc.ListTerminalSessionRunsNeedingContextLifecycleRow, error) {
	return nil, nil
}

func (stubAgentQueries) GetDiscussSession(context.Context, dbsqlc.GetDiscussSessionParams) (dbsqlc.GetDiscussSessionRow, error) {
	return dbsqlc.GetDiscussSessionRow{}, pgx.ErrNoRows
}

func (stubAgentQueries) SaveSessionRunResumeContext(context.Context, dbsqlc.SaveSessionRunResumeContextParams) (int64, error) {
	return 1, nil
}

func (stubAgentQueries) ListUserInputsByRun(context.Context, pgtype.UUID) ([]dbsqlc.UserInputRequest, error) {
	return nil, nil
}

type (
	stubAgentMessages  struct{ messagepkg.Service }
	stubAgentSessions  struct{ runreq.SessionService }
	denyAllPermissions struct{}
)

func (denyAllPermissions) HasBotPermission(context.Context, string, string, string) (bool, error) {
	return false, nil
}

type stubAgentWorkspaceTargets struct{}

func (stubAgentWorkspaceTargets) ResolveWorkspaceTarget(context.Context, string, string) (workspace.ResolvedWorkspaceTarget, error) {
	return workspace.ResolvedWorkspaceTarget{}, nil
}

// newTestAgentService builds an application service through New. Required
// dependencies the test leaves zero take stand-ins, and the session runtime
// defaults to a fresh in-memory manager.
func newTestAgentService(t *testing.T, d application.Deps) *application.Service {
	t.Helper()
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Queries == nil {
		d.Queries = stubAgentQueries{}
	}
	if d.Messages == nil {
		d.Messages = stubAgentMessages{}
	}
	if d.Sessions == nil {
		d.Sessions = stubAgentSessions{}
	}
	if d.Settings == nil {
		d.Settings = &settings.Service{}
	}
	if d.Accounts == nil {
		d.Accounts = &accounts.Service{}
	}
	if d.Models == nil {
		d.Models = &models.Service{}
	}
	if d.Agent == nil {
		d.Agent = &native.Agent{}
	}
	if d.SessionRuntime == nil {
		d.SessionRuntime = sessionruntime.NewManager(sessionruntime.NewMemoryBackend(), sessionruntime.Options{})
		t.Cleanup(func() { _ = d.SessionRuntime.Close() })
	}
	if d.BotPermissions == nil {
		d.BotPermissions = denyAllPermissions{}
	}
	if d.WorkspaceTargets == nil {
		d.WorkspaceTargets = stubAgentWorkspaceTargets{}
	}
	if d.ToolApproval == nil {
		d.ToolApproval = &toolapproval.Service{}
	}
	if d.UserInput == nil {
		d.UserInput = &userinput.Service{}
	}
	if d.ResumeSecret == "" {
		d.ResumeSecret = "handlers-test-secret"
	}
	service, err := application.New(d)
	if err != nil {
		t.Fatalf("application.New: %v", err)
	}
	return service
}
