package transcript

import (
	"context"
	"errors"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// ContinuationRequest is the request row a continuation of runHandle answers
// and the turn that row belongs to. An applied steer opens a later turn within
// the run. Its next tool result and resumed model output belong there, even
// though admission's run handle still names the original request turn. A run
// always names its turn (session_runs.turn_id is not null), so a handle without
// one is an error, and a run whose request row is not in history gets
// messagepkg.ErrRunRequestNotFound.
func (t *Reader) ContinuationRequest(ctx context.Context, sessionID string, runHandle sessionruntime.RunHandle) (messagepkg.RunTurnRequest, error) {
	turnID := strings.TrimSpace(runHandle.TurnID)
	if turnID == "" {
		return messagepkg.RunTurnRequest{}, errors.New("continuation run handle names no turn")
	}
	return t.messages.GetRunTurnRequest(ctx, sessionID, strings.TrimSpace(runHandle.RunID), turnID)
}
