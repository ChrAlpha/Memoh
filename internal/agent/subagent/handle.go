package subagent

import (
	"context"
	"log/slog"
	"sync"

	"github.com/felinics/memoh/internal/agent/outcome"
	"github.com/felinics/memoh/internal/agent/persist"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/errs"
)

// runKey carries the admitted run through the context Admit hands back to the
// tool layer. The tool layer never reads it: it treats the context as opaque.
// The step committer and the runtime event observer installed on the spawn path
// need the run's identity (RunID, FencingToken) to write and publish under the
// right run, and the run's ending needs the committer the attempts wrote with.
type runKey struct{}

// spawnedRun is what the context carries for one admitted run.
type spawnedRun struct {
	handle sessionruntime.RunHandle

	mu sync.Mutex
	// committer is the last attempt's. An attempt is retried only before it
	// wrote anything, so earlier attempts have nothing to name.
	committer *persist.StepCommitter
}

func withHandle(ctx context.Context, handle sessionruntime.RunHandle) context.Context {
	return context.WithValue(ctx, runKey{}, &spawnedRun{handle: handle})
}

func runFromContext(ctx context.Context) (*spawnedRun, bool) {
	if ctx == nil {
		return nil, false
	}
	run, ok := ctx.Value(runKey{}).(*spawnedRun)
	return run, ok
}

// HandleFromContext returns the run handle Admit stored on the context, if the
// context belongs to an admitted subagent run.
func HandleFromContext(ctx context.Context) (sessionruntime.RunHandle, bool) {
	run, ok := runFromContext(ctx)
	if !ok {
		return sessionruntime.RunHandle{}, false
	}
	return run.handle, true
}

func (r *spawnedRun) useCommitter(c *persist.StepCommitter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.committer = c
}

func (r *spawnedRun) currentCommitter() *persist.StepCommitter {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.committer
}

// endHistory ends the history of the attempt that is ending: the failure it was
// named with goes onto the checkpoint it wrote, then the committer closes. An
// attempt with no declared failure was stopped or completed and names nothing.
// A write that fails is logged: the run has ended and nobody is left to act on it.
func (r *spawnedRun) endHistory(ctx context.Context, declared *outcome.RunFailure, logger *slog.Logger) {
	committer := r.currentCommitter()
	if committer == nil {
		return
	}
	if declared != nil {
		if err := committer.NameCheckpoint(ctx, *declared); err != nil {
			logger.ErrorContext(ctx, "subagent checkpoint was not named", slog.Any("error", errs.Recorded(err)))
		}
	}
	if err := committer.Finish(ctx, 0); err != nil {
		logger.ErrorContext(ctx, "subagent history was not closed", slog.Any("error", errs.Recorded(err)))
	}
}
