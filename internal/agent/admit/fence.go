package admit

import (
	"context"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/runtimefence"
)

// Fence carries the fencing token of an admitted run on ctx, for a runtime that
// keeps its runs durably; any other runtime has none.
func (a *Admitter) Fence(ctx context.Context, admission sessionruntime.Admission) context.Context {
	if !a.usesDurableTerminalObserver() {
		return ctx
	}
	return runtimefence.WithContext(ctx, runtimefence.Fence{
		BotID:     admission.Handle.BotID,
		SessionID: admission.Handle.SessionID,
		Token:     admission.Handle.FencingToken,
	})
}

func (a *Admitter) usesDurableTerminalObserver() bool {
	_, durable := a.runtime.(*sessionruntime.Manager)
	return durable
}
