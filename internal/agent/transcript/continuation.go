package transcript

import (
	"context"
	"strings"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

type continuationTurnRequestMessageResolver interface {
	GetVisibleHistoryTurnRequestMessageIDByTurn(context.Context, string, string) (string, error)
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
	return resolver.GetVisibleHistoryTurnRequestMessageIDByTurn(ctx, sessionID, turnID)
}
