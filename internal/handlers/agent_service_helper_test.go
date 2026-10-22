package handlers

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/accounts"
	"github.com/felinics/memoh/internal/agent"
	"github.com/felinics/memoh/internal/agent/agentapi"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	userinput "github.com/felinics/memoh/internal/agent/decision/input"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/turn"
	chatview "github.com/felinics/memoh/internal/agent/view"
	"github.com/felinics/memoh/internal/agent/web"
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

func (stubAgentQueries) GetSessionInterruptedRun(context.Context, pgtype.UUID) (dbsqlc.SessionRun, error) {
	return dbsqlc.SessionRun{}, pgx.ErrNoRows
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
	stubAgentMessages struct {
		messagepkg.Service
		messagepkg.AgentStepPersister
	}
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
func newTestAgentService(t *testing.T, d agent.Deps) *agent.Service {
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
	if d.RunCredentialSecret == "" {
		d.RunCredentialSecret = "handlers-test-secret"
	}
	service, err := agent.New(d)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return service
}

// inertAgent stands in for the agent service in handler tests that never reach
// it: every call fails the test loudly instead of acting on a zero service.
type inertAgent struct{}

func (inertAgent) fail() { panic("handler test reached the agent service") }

func (a inertAgent) AdmitWebRun(context.Context, web.AdmitInput) (*web.Admission, error) {
	a.fail()
	return nil, nil
}
func (a inertAgent) RunWebSend(*web.Admission, runreq.ChatRequest, agentapi.RunDelivery) { a.fail() }
func (a inertAgent) RunWebRetry(*web.Admission, web.RetryLatestMessageInput, agentapi.RunDelivery) {
	a.fail()
}

func (a inertAgent) RunWebEdit(*web.Admission, web.EditLatestMessageInput, agentapi.RunDelivery) {
	a.fail()
}

func (a inertAgent) PrepareRetryLatestTurnOperation(context.Context, string, string) (string, error) {
	a.fail()
	return "", nil
}

func (a inertAgent) PrepareRetryRequestUserTurn(context.Context, string, string) (*chatview.UITurn, error) {
	a.fail()
	return nil, nil
}

func (a inertAgent) PrepareEditLatestTurnOperation(context.Context, string, string) (string, error) {
	a.fail()
	return "", nil
}

func (a inertAgent) ResolveTurnIDForMessage(context.Context, string, string) (string, error) {
	a.fail()
	return "", nil
}

func (a inertAgent) ApplyUserMessageHookAndPersistUserTurn(context.Context, runreq.ChatRequest) (runreq.ChatRequest, messagepkg.Message, error) {
	a.fail()
	return runreq.ChatRequest{}, messagepkg.Message{}, nil
}

func (a inertAgent) ValidateWorkspaceTarget(context.Context, string, string) error {
	a.fail()
	return nil
}

func (a inertAgent) ReconcileSessionModelPreference(context.Context, string, string, string) (string, string, error) {
	a.fail()
	return "", "", nil
}

func (a inertAgent) EnqueueSteer(context.Context, agentapi.QueueInput) (sessionruntime.SteerItem, error) {
	a.fail()
	return sessionruntime.SteerItem{}, nil
}

func (a inertAgent) EnqueueFollowUp(context.Context, agentapi.QueueInput) (sessionruntime.FollowUpItem, error) {
	a.fail()
	return sessionruntime.FollowUpItem{}, nil
}

func (a inertAgent) ListSessionQueues(context.Context, string, string) (agentapi.SessionQueues, error) {
	a.fail()
	return agentapi.SessionQueues{}, nil
}

func (a inertAgent) ReorderSteer(context.Context, string, string, sessionruntime.SteerPendingRef, sessionruntime.SteerPendingRef) ([]sessionruntime.SteerItem, error) {
	a.fail()
	return nil, nil
}

func (a inertAgent) UpdateSteer(context.Context, string, string, string, string, string) (sessionruntime.SteerItem, error) {
	a.fail()
	return sessionruntime.SteerItem{}, nil
}

func (a inertAgent) CancelSteer(context.Context, string, string, string) error {
	a.fail()
	return nil
}

func (a inertAgent) ReorderFollowUp(context.Context, string, string, sessionruntime.FollowUpPendingRef, sessionruntime.FollowUpPendingRef) ([]sessionruntime.FollowUpItem, error) {
	a.fail()
	return nil, nil
}

func (a inertAgent) UpdateFollowUp(context.Context, string, string, string, string, string) (sessionruntime.FollowUpItem, error) {
	a.fail()
	return sessionruntime.FollowUpItem{}, nil
}

func (a inertAgent) CancelFollowUp(context.Context, string, string, string) error {
	a.fail()
	return nil
}

func (a inertAgent) PromoteFollowUpToSteer(context.Context, string, string, sessionruntime.FollowUpPendingRef) (sessionruntime.PromoteFollowUpResult, error) {
	a.fail()
	return sessionruntime.PromoteFollowUpResult{}, nil
}

func (a inertAgent) RuntimeControls(context.Context, turn.RuntimeControlRequest) (external.Controls, error) {
	a.fail()
	return external.Controls{}, nil
}

func (a inertAgent) SetRuntimeMode(context.Context, turn.RuntimeControlRequest) (external.ModeState, error) {
	a.fail()
	return external.ModeState{}, nil
}

func (a inertAgent) ExecuteRuntimeCommand(context.Context, turn.RuntimeControlRequest) (turn.RuntimeCommandResult, error) {
	a.fail()
	return turn.RuntimeCommandResult{}, nil
}

func (a inertAgent) AbortRuntimeRun(context.Context, string, string, string, string) (bool, error) {
	a.fail()
	return false, nil
}

func (a inertAgent) RuntimeSessionExecutionInfo(context.Context, string) (agentapi.SessionExecutionInfo, error) {
	a.fail()
	return agentapi.SessionExecutionInfo{}, nil
}

func (a inertAgent) ViewerCanDecideToolApproval(context.Context, string, agentapi.ToolApprovalPrincipal) (bool, error) {
	a.fail()
	return false, nil
}

var (
	_ webRunService         = inertAgent{}
	_ queueService          = inertAgent{}
	_ runtimeControlService = inertAgent{}
	_ decisionService       = inertAgent{}
)
