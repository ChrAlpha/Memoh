package admit

import (
	"context"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/runtimefence"
)

// fencingRuntime is a runtime that keeps its runs durably and so fences them.
type fencingRuntime interface {
	FencesRuns() bool
}

// Fence carries the fencing token of an admitted run on ctx, for a runtime that
// fences its runs; any other runtime has none.
func (a *Admitter) Fence(ctx context.Context, admission sessionruntime.Admission) context.Context {
	if !a.fencesRuns() {
		return ctx
	}
	return runtimefence.WithContext(ctx, runtimefence.Fence{
		BotID:     admission.Handle.BotID,
		SessionID: admission.Handle.SessionID,
		Token:     admission.Handle.FencingToken,
	})
}

func (a *Admitter) fencesRuns() bool {
	fencing, ok := a.runtime.(fencingRuntime)
	return ok && fencing.FencesRuns()
}
