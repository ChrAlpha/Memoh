package handlers

import (
	"context"

	"github.com/felinics/memoh/internal/agent/decide"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/externalrun"
	"github.com/felinics/memoh/internal/agent/pending"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/external"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/title"
	"github.com/felinics/memoh/internal/agent/turn"
	"github.com/felinics/memoh/internal/agent/web"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// The agent service is injected into the handlers as one interface per role,
// so a handler names what it needs from the service and a test supplies only
// that. *agent.Service satisfies all of them.

// webRunService is what the Web socket needs to admit and run a send, a retry
// or an edit, and to check what it carries.
type webRunService interface {
	AdmitWebRun(ctx context.Context, in web.AdmitInput) (*web.Admission, error)
	RunWebSend(admission *web.Admission, req runreq.ChatRequest, delivery drive.RunDelivery)
	RunWebRetry(admission *web.Admission, input web.RetryLatestMessageInput, delivery drive.RunDelivery)
	RunWebEdit(admission *web.Admission, input web.EditLatestMessageInput, delivery drive.RunDelivery)
	PrepareRetryLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error)
	PrepareEditLatestTurnOperation(ctx context.Context, sessionID, turnID string) (string, error)
	ResolveTurnIDForMessage(ctx context.Context, sessionID, messageID string) (string, error)
	ApplyUserMessageHookAndPersistUserTurn(ctx context.Context, req runreq.ChatRequest) (runreq.ChatRequest, messagepkg.Message, error)
	ValidateWorkspaceTarget(ctx context.Context, botID, targetID string) error
	ReconcileSessionModelPreference(ctx context.Context, botID, modelRef, effort string) (string, string, error)
}

// queueEnqueuer is what the chat socket needs to queue a text behind a run.
type queueEnqueuer interface {
	EnqueueSteer(ctx context.Context, input pending.QueueInput) (sessionruntime.SteerItem, error)
	EnqueueFollowUp(ctx context.Context, input pending.QueueInput) (sessionruntime.FollowUpItem, error)
}

// queueService is the queue of a session as the REST handlers operate it.
type queueService interface {
	queueEnqueuer
	ListSessionQueues(ctx context.Context, botID, sessionID string) (pending.SessionQueues, error)
	ReorderSteer(ctx context.Context, botID, sessionID string, item, before sessionruntime.SteerPendingRef) ([]sessionruntime.SteerItem, error)
	UpdateSteer(ctx context.Context, botID, sessionID, itemID, editorUserID, text string) (sessionruntime.SteerItem, error)
	CancelSteer(ctx context.Context, botID, sessionID, itemID string) error
	ReorderFollowUp(ctx context.Context, botID, sessionID string, item, before sessionruntime.FollowUpPendingRef) ([]sessionruntime.FollowUpItem, error)
	UpdateFollowUp(ctx context.Context, botID, sessionID, itemID, editorUserID, text string) (sessionruntime.FollowUpItem, error)
	CancelFollowUp(ctx context.Context, botID, sessionID, itemID string) error
	PromoteFollowUpToSteer(ctx context.Context, botID, sessionID string, ref sessionruntime.FollowUpPendingRef) (sessionruntime.PromoteFollowUpResult, error)
}

// runtimeControlService is what the chat socket asks of a session's runtime:
// its controls, mode and commands, its execution info, and aborting a run.
type runtimeControlService interface {
	RuntimeControls(ctx context.Context, request turn.RuntimeControlRequest) (external.Controls, error)
	SetRuntimeMode(ctx context.Context, request turn.RuntimeControlRequest) (external.ModeState, error)
	ExecuteRuntimeCommand(ctx context.Context, request turn.RuntimeControlRequest) (turn.RuntimeCommandResult, error)
	AbortRuntimeRun(ctx context.Context, botID, sessionID, runID, controlID string) (bool, error)
	RuntimeSessionExecutionInfo(ctx context.Context, sessionID string) (externalrun.SessionExecutionInfo, error)
}

// decisionService answers whether a viewer may decide a tool approval.
type decisionService interface {
	ViewerCanDecideToolApproval(ctx context.Context, approvalID string, principal decide.ToolApprovalPrincipal) (bool, error)
}

// starterPromptService produces the new-chat suggestions.
type starterPromptService interface {
	StarterPrompts(ctx context.Context, req title.StarterPromptsRequest) (title.StarterPromptsResult, error)
}
