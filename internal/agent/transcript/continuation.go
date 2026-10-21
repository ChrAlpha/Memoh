package transcript

import (
	"context"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

// ContinuationRequest is the request row a continuation of runHandle answers
// and the turn that row belongs to. An applied steer opens a later turn within
// the run. Its next tool result and resumed model output belong there, even
// though admission's run handle still names the original request turn. The
// result is empty when the run names no turn; a run whose request row is not in
// history gets messagepkg.ErrRunRequestNotFound.
func (t *Reader) ContinuationRequest(ctx context.Context, sessionID string, runHandle sessionruntime.RunHandle) (messagepkg.RunTurnRequest, error) {
	turnID := strings.TrimSpace(runHandle.TurnID)
	if turnID == "" {
		return messagepkg.RunTurnRequest{}, nil
	}
	return t.messages.GetRunTurnRequest(ctx, sessionID, strings.TrimSpace(runHandle.RunID), turnID)
}
