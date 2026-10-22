package plan

import (
	"context"
	"errors"

	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/persist"
	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// stepMode is how a native run commits its steps.
type stepMode int

const (
	// stepsFresh commits each step through a committer of the run. The run must
	// have what one needs: a fence, a message store that writes steps, a named
	// turn.
	stepsFresh stepMode = iota
	// stepsResume commits through the live queue the run was admitted with,
	// numbering the steps after the ones the parked run already committed.
	stepsResume
)

// errStepCommitterUnavailable is why a native run that commits steps cannot be
// planned: the history of a run is written only by its step committer, so a run
// that cannot have one does not start.
var errStepCommitterUnavailable = errors.New("step committer is unavailable for the run")

// nativeOptions are the ways the entries of a native run differ at its tail.
type nativeOptions struct {
	// LiveToolStream says someone follows the run's tool calls as they happen.
	LiveToolStream bool
	// CanRequestUserInput says the run may park on a question, which only a run
	// someone follows live can have answered.
	CanRequestUserInput bool
	// Steps is how the run commits its steps.
	Steps stepMode
}

// nativePlan is the tail every native run shares: it applies the run's flags,
// binds the step commits and the reasoning timing, compiles the prompt,
// binds the compaction between model calls and builds the executor. cfg is the
// run's configuration as the entry shaped it (the prompt it runs with, the
// identity it runs as, the context it assembled), and rc is the resolution
// the history writes read.
func (p *Planner) nativePlan(ctx context.Context, req runreq.ChatRequest, rc runreq.ResolvedContext, cfg native.RunConfig, opts nativeOptions) (drive.RunPlan, error) {
	cfg.LiveToolStream = opts.LiveToolStream
	cfg.CanRequestUserInput = opts.CanRequestUserInput

	reasoningTiming := persist.NewReasoningTimingTracker(nil)
	var stepCommitter *persist.StepCommitter
	if opts.Steps == stepsResume {
		var err error
		if stepCommitter, err = p.resumeCommitter(ctx, &req, &cfg, rc); err != nil {
			return drive.RunPlan{}, err
		}
	} else if stepCommitter = p.persist.NewStepCommitter(ctx, req, rc); stepCommitter == nil {
		return drive.RunPlan{}, errStepCommitterUnavailable
	}
	persist.ConfigureNativeReasoningTiming(&cfg, reasoningTiming, stepCommitter)
	stepCommitter.BindContinuation(&cfg)
	cfg = p.runconfig.Prepare(ctx, cfg)
	p.compact.BindStep(&cfg, req, stepCommitter)

	return drive.RunPlan{
		Exec: &nativeExecutor{
			agent:     p.agent,
			cfg:       cfg,
			rc:        rc,
			committer: stepCommitter,
		},
		Req:        req,
		IdleEffort: drive.ReasoningEffortForIdle(cfg),
		Lifecycle:  p.lifecycle.Terminal(ctx, cfg),
	}, nil
}
