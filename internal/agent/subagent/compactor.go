package subagent

import (
	"context"

	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// StepCompactor is the between-calls compactor of a spawned run whose
// steps StepCommit commits to its thread as they complete.
func (h *Host) StepCompactor(ctx context.Context, run native.SpawnCompaction) native.StepContextCompactor {
	handle, ok := HandleFromContext(ctx)
	if !ok {
		return nil
	}
	return h.compact.SubagentStep(run, handle.RunID)
}
