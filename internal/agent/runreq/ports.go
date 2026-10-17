package runreq

import (
	"context"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	session "github.com/felinics/memoh/internal/chat/thread"
)

// BotPermissionChecker answers whether an account holds a permission on a bot.
type BotPermissionChecker interface {
	HasBotPermission(ctx context.Context, botID, accountID, permission string) (bool, error)
}

// SessionService is the interface the application service uses for session metadata updates.
type SessionService interface {
	Get(ctx context.Context, sessionID string) (session.Thread, error)
	UpdateTitle(ctx context.Context, sessionID, title string) (session.Thread, error)
	UpdateMetadata(ctx context.Context, sessionID string, metadata map[string]any) (session.Thread, error)
	MergeRuntimeMetadata(ctx context.Context, sessionID, runtimeType string, delta map[string]any) (session.Thread, error)
}

// TurnAdmitter is the durable admission gate StartTurn depends on.
//
// It is an interface so this package can be tested for what it actually owns —
// translating the runtime's answers into the turn port's vocabulary and closing
// the run's record when it ends — without standing up a ledger. What admission
// itself guarantees is tested where it is implemented; restating it against a
// stub here would only assert that the stub agrees with itself.
type TurnAdmitter interface {
	Admit(context.Context, sessionruntime.AdmitInput) (sessionruntime.Admission, error)
	FinishRunWithErrorCode(ctx context.Context, handle sessionruntime.RunHandle, status, errorCode string) (sessionruntime.TerminalRun, error)
	FinishRunWithFailure(ctx context.Context, handle sessionruntime.RunHandle, status, errorCode string, errorArgs map[string]string) (sessionruntime.TerminalRun, error)
	// MarkInlineDecisionRun declares an admitted run's decision semantics:
	// its runtime blocks inline on decisions, so terminal decision statuses
	// resume the run (external drivers). Native runs skip the declaration
	// and resume only through their re-entering stream.
	MarkInlineDecisionRun(botID, sessionID, runID string)
}
