// Package decide resolves the decisions a run parks on: tool approvals and the
// questions of ask_user. It decides who may answer one, records the answer,
// closes the decisions of a run that ended or was lost, and hands the answered
// call back for whoever resumes the run.
package decide

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/felinics/memoh/internal/agent/activeprompt"
	toolapproval "github.com/felinics/memoh/internal/agent/decision/approval"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	chatview "github.com/felinics/memoh/internal/agent/view"
	sessionpkg "github.com/felinics/memoh/internal/chat/thread"
	"github.com/felinics/memoh/internal/db/postgres/sqlc"
)

// Queries is the part of the database the decisions are read through.
type Queries interface {
	GetSessionRun(ctx context.Context, id pgtype.UUID) (sqlc.SessionRun, error)
	GetToolApprovalRequest(ctx context.Context, id pgtype.UUID) (sqlc.ToolApprovalRequest, error)
	GetUserInputRequest(ctx context.Context, id pgtype.UUID) (sqlc.UserInputRequest, error)
	ListPendingToolApprovalsByRun(ctx context.Context, runID pgtype.UUID) ([]sqlc.ToolApprovalRequest, error)
	ListPendingUserInputsByRun(ctx context.Context, runID pgtype.UUID) ([]sqlc.UserInputRequest, error)
	ListToolApprovalsByRun(ctx context.Context, runID pgtype.UUID) ([]sqlc.ToolApprovalRequest, error)
	ListUserInputsByRun(ctx context.Context, runID pgtype.UUID) ([]sqlc.UserInputRequest, error)
}

// Sessions reads the session a decision belongs to.
type Sessions interface {
	Get(ctx context.Context, sessionID string) (sessionpkg.Thread, error)
}

// Events publishes a decision's final state to the run's live view.
type Events interface {
	HandleAgentEvent(ctx context.Context, handle sessionruntime.RunHandle, event native.StreamEvent) ([]chatview.UIMessage, error)
}

// Limits is where the size limit on a tool result comes from.
type Limits interface {
	Limits() native.Limits
}

// Deps is everything New needs.
type Deps struct {
	// Logger receives the decider's own records.
	Logger *slog.Logger
	// Queries reads the decision rows and the run ledger.
	Queries Queries
	// Sessions reads the session of a decision.
	Sessions Sessions
	// ToolApproval stores and resolves tool approvals.
	ToolApproval *toolapproval.Service
	// UserInput stores and resolves ask_user questions.
	UserInput UserInputs
	// BotPermissions decides whether an account may act on a bot.
	BotPermissions runreq.BotPermissionChecker
	// ActivePrompts is where a response stream reattaches to the live prompt
	// of an external agent.
	ActivePrompts *activeprompt.Registry
	// Events publishes the final state of a run's decisions.
	Events Events
	// Limits sizes the tool results a decision produces.
	Limits Limits
}

// Decider resolves decisions.
type Decider struct {
	logger         *slog.Logger
	queries        Queries
	sessions       Sessions
	toolApproval   *toolapproval.Service
	userInput      UserInputs
	botPermissions runreq.BotPermissionChecker
	activePrompts  *activeprompt.Registry
	events         Events
	limits         Limits
}

// New returns a Decider. A nil Logger discards the decider's records.
func New(d Deps) *Decider {
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Decider{
		logger:         logger,
		queries:        d.Queries,
		sessions:       d.Sessions,
		toolApproval:   d.ToolApproval,
		userInput:      d.UserInput,
		botPermissions: d.BotPermissions,
		activePrompts:  d.ActivePrompts,
		events:         d.Events,
		limits:         d.Limits,
	}
}
