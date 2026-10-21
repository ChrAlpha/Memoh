package transcript

import (
	"context"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	messagepkg "github.com/felinics/memoh/internal/chat/message"
)

type continuationTurnRequestResolver interface {
	GetRunTurnRequest(context.Context, string, string, string) (messagepkg.RunTurnRequest, error)
}

// ContinuationRequest is the request row a continuation of runHandle answers
// and the turn that row belongs to. An applied steer opens a later turn within
// the run. Its next tool result and resumed model output belong there, even
// though admission's run handle still names the original request turn. The
// result is empty when the run names no turn or the store cannot resolve one.
func (t *Reader) ContinuationRequest(ctx context.Context, sessionID string, runHandle sessionruntime.RunHandle) (messagepkg.RunTurnRequest, error) {
	turnID := strings.TrimSpace(runHandle.TurnID)
	if turnID == "" {
		return messagepkg.RunTurnRequest{}, nil
	}
	resolver, ok := t.messages.(continuationTurnRequestResolver)
	if !ok {
		return messagepkg.RunTurnRequest{}, nil
	}
	return resolver.GetRunTurnRequest(ctx, sessionID, strings.TrimSpace(runHandle.RunID), turnID)
}
