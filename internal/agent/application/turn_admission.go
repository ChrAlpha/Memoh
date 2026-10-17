package application

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/felinics/memoh/internal/agent/admit"
	"github.com/felinics/memoh/internal/agent/drive"
	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
	"github.com/felinics/memoh/internal/agent/sessionmode"
	tools "github.com/felinics/memoh/internal/agent/tool"
	"github.com/felinics/memoh/internal/agent/turn"
)

// bindSessionRuntime attaches the durable admission gate and registers the
// callbacks the runtime invokes into this service. The manager is built before
// the service and holds no reference back to it, so New can wire both ways.
func (s *Service) bindSessionRuntime(manager *sessionruntime.Manager) {
	s.sessionRuntime = manager
	s.sessionManager = manager
	s.decisionRuntime = manager
	s.abortRuntime = manager
	manager.SetDecisionStore(s.decider)
	manager.SetLostRunDecisionCanceller(s.decider.CancelLostRunDecisions)
	manager.SetCommandHandler(s.handleRuntimeDecisionCommand)
	manager.SetDecisionFinalizer(s.decider.FinalizeRuntimeDecisions)
	manager.SetTerminalObserver(terminalFanout(
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			outcome.LogRunResult(ctx, s.logger, terminal)
		},
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			s.lifecycle.ReconcileTerminal(ctx, terminal)
		},
		s.afterSessionTerminal,
	))
	manager.SetTerminalReconciler(func(ctx context.Context) error {
		return s.lifecycle.ReconcileTerminals(ctx)
	})
}

// terminalFanout runs the terminal observers in the order given. The order is
// the contract: the outcome record comes first, the lifecycle reconciliation
// next, and whatever the run's end releases (queued input) last.
func terminalFanout(steps ...func(context.Context, sessionruntime.TerminalRun)) func(context.Context, sessionruntime.TerminalRun) {
	return func(ctx context.Context, terminal sessionruntime.TerminalRun) {
		for _, step := range steps {
			step(ctx, terminal)
		}
	}
}

// AdmitSubagentRun puts a spawned agent's turn through the same durable
// admission every other turn takes, and answers in the vocabulary of the turn
// port so the tool layer never sees a runtime type.
//
// The slot a subagent takes is its own thread's, not its parent's: a parent may
// have several agents working at once, and each of those threads still runs one
// turn at a time. Busy therefore means *this agent* is already working — a fact
// the parent model can act on — rather than a failure to report.
func (s *Service) AdmitSubagentRun(
	ctx context.Context,
	botID, threadID, invocationID string,
	submission []byte,
) (context.Context, tools.SubagentAdmission, func(tools.SubagentTerminal), error) {
	endActiveTurn, beginErr := s.activeTurns.Begin()
	if beginErr != nil {
		return nil, tools.SubagentAdmission{}, nil, beginErr
	}
	runCtx, admission, release, err := s.admit.Triggered(ctx, admit.Trigger{
		BotID: botID, ThreadID: threadID, InvocationID: invocationID,
		Submission: submission, StopCause: context.Canceled,
	})
	if err != nil {
		endActiveTurn()
	}
	switch {
	case errors.Is(err, sessionruntime.ErrSessionBusy):
		return nil, tools.SubagentAdmission{}, nil, fmt.Errorf("%w: thread %s", turn.ErrSessionBusy, threadID)
	case errors.Is(err, sessionruntime.ErrInvocationConflict):
		// This task already has a run. Executing it again would answer one
		// message twice, so it is dropped exactly like a channel redelivery.
		return nil, tools.SubagentAdmission{}, nil, fmt.Errorf("%w: %s", turn.ErrDuplicateTurn, invocationID)
	case err != nil:
		return nil, tools.SubagentAdmission{}, nil, fmt.Errorf("admit subagent turn: %w", err)
	}
	// The durable fence is installed during triggered admission. The opaque run
	// handle also travels on the context so spawned step persistence and live
	// observation can use it without exposing runtime types through this port.
	runCtx = withSubagentRunHandle(runCtx, admission.Handle)
	toolAdmission := tools.SubagentAdmission{
		RunID:        admission.RunID,
		TurnID:       admission.TurnID,
		TurnPosition: admission.TurnPosition,
	}
	var once sync.Once
	terminal := func(result tools.SubagentTerminal) {
		once.Do(func() {
			defer endActiveTurn()
			lifecycleCause := result.Cause
			if lifecycleCause == nil && runCtx.Err() != nil &&
				(!result.OutcomeResolved || result.Outcome != tools.SpawnAttemptCompleted) {
				lifecycleCause = context.Cause(runCtx)
			}
			status := ""
			if result.OutcomeResolved {
				switch result.Outcome {
				case tools.SpawnAttemptCompleted:
					status = sessionruntime.RunStatusCompleted
				case tools.SpawnAttemptAbort:
					status = sessionruntime.RunStatusAborted
				case tools.SpawnAttemptFailure:
					status = sessionruntime.RunStatusErrored
				}
			}
			s.lifecycle.PersistSnapshot(
				runCtx,
				admission.RunID,
				botID,
				threadID,
				result.ContextLifecycle,
				lifecycleCause,
				true,
			)
			defer release()
			s.driver.FinishRun(runCtx, drive.AdmittedRun{Handle: admission.Handle, Mode: sessionmode.Subagent},
				drive.FromOutcome(outcome.RunOutcome{Status: status, Cause: result.Cause}.Resolve(runCtx)))
		})
	}
	return runCtx, toolAdmission, terminal, nil
}
