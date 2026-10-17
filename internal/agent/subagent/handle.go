package subagent

import (
	"context"

	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// handleKey carries the admitted run's handle through the context Admit hands
// back to the tool layer. The tool layer never reads it: it treats the context
// as opaque. The step committer and the runtime event observer installed on the
// spawn path need the run's identity (RunID, FencingToken) to write and publish
// under the right run.
type handleKey struct{}

func withHandle(ctx context.Context, handle sessionruntime.RunHandle) context.Context {
	return context.WithValue(ctx, handleKey{}, handle)
}

// HandleFromContext returns the run handle Admit stored on the context, if the
// context belongs to an admitted subagent run.
func HandleFromContext(ctx context.Context) (sessionruntime.RunHandle, bool) {
	if ctx == nil {
		return sessionruntime.RunHandle{}, false
	}
	handle, ok := ctx.Value(handleKey{}).(sessionruntime.RunHandle)
	return handle, ok
}
