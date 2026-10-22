package subagent

import (
	"context"
	"errors"
	"strings"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
	"github.com/felinics/memoh/internal/agent/step"
)

// StepCommit returns the per-step persistence callbacks for one spawned agent
// run. They belong to the step committer every run's history is written by: the
// run is the same admitted, fenced turn, filed under the task's request row, and
// it takes none of what a spawned run has no use for. It has no queue to steer
// from, no turn to replace, no memory to extract and no compaction to trigger
// when it ends. The run must be admitted and fenced and its task request row
// written; admission guarantees all three, so a context or request that lacks one
// is an error and the run does not start. onPersisted fires after either kind of
// durable write; the spawn provider uses it to stop retrying an attempt whose
// output is already part of history.
func (h *Host) StepCommit(
	ctx context.Context,
	botID, sessionID, modelID, turnRequestMessageID string,
	contextLifecycle *contextfrag.LifecycleHolder,
	onPersisted func(),
) (
	func(context.Context, int, *step.Record) error,
	func(context.Context, int, *step.Record) error,
	error,
) {
	run, ok := runFromContext(ctx)
	if !ok || strings.TrimSpace(run.handle.RunID) == "" || run.handle.FencingToken <= 0 {
		return nil, nil, errors.New("subagent step commit requires an admitted run handle")
	}
	// Steps are filed under the task's request row; without it they would belong
	// to no turn.
	requestID := strings.TrimSpace(turnRequestMessageID)
	if requestID == "" {
		return nil, nil, errors.New("subagent step commit requires the task request row")
	}
	req := runreq.ChatRequest{
		BotID:                  botID,
		ThreadID:               sessionID,
		RunID:                  run.handle.RunID,
		RunHandle:              run.handle,
		UserMessagePersisted:   true,
		PersistedUserMessageID: requestID,
		SkipMemoryExtraction:   true,
	}
	rc := runreq.ResolvedContext{RunConfig: native.RunConfig{ContextLifecycle: contextLifecycle}}
	rc.Model.ID = modelID
	committer := h.persist.NewStepCommitter(ctx, req, rc, persist.OnStepPersisted(onPersisted))
	if committer == nil {
		return nil, nil, errors.New("subagent step commit requires a fenced run, bot and session ids and a message store that writes steps")
	}
	run.useCommitter(committer)
	commit, interrupt := committer.Hooks()
	return commit, interrupt, nil
}
