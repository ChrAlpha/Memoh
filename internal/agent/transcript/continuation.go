package transcript

import (
	"context"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

type continuationTurnRequestMessageResolver interface {
	GetRunTurnRequestMessageID(context.Context, string, string, string) (string, error)
}

func (t *Reader) ContinuationRequestMessageID(ctx context.Context, sessionID string, runHandle sessionruntime.RunHandle) (string, error) {
	turnID := strings.TrimSpace(runHandle.TurnID)
	if turnID == "" {
		return "", nil
	}
	resolver, ok := t.messages.(continuationTurnRequestMessageResolver)
	if !ok {
		return "", nil
	}
	// An applied steer opens a later turn within this run. Its next tool
	// result and resumed model output belong there, even though admission's
	// run handle still names the original request turn.
	return resolver.GetRunTurnRequestMessageID(ctx, sessionID, strings.TrimSpace(runHandle.RunID), turnID)
}
