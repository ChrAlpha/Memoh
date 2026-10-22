package plan

import (
	"context"
	"errors"

	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

var errResumeCommitterUnavailable = errors.New("live queue step committer is unavailable for decision continuation")

// resumeCommitter installs the live queue step boundary the parked run was
// admitted with onto its continuation. Queue items are transient; history
// remains persisted by the normal message service, while claims are fenced by
// the session runtime backend. A continuation whose handle does not name an
// owned run has no queue, so it cannot be resumed.
func (p *Planner) resumeCommitter(ctx context.Context, req *runreq.ChatRequest, cfg *native.RunConfig, rc runreq.ResolvedContext) (*persist.StepCommitter, error) {
	if req.RunHandle.RunID == "" || req.RunHandle.OwnerID == "" || req.RunHandle.FencingToken <= 0 {
		return nil, errResumeCommitterUnavailable
	}

	stepIndex, err := p.runtime.ContinuationStepIndex(req.RunHandle)
	if err != nil {
		return nil, err
	}
	req.StepIndexOffset = stepIndex
	cfg.StepIndexOffset = stepIndex

	req.QueueSteerEnabled = true
	committer := p.persist.NewStepCommitter(ctx, *req, rc)
	if committer == nil {
		return nil, errResumeCommitterUnavailable
	}
	return committer, nil
}
