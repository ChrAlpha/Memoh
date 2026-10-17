package application

import (
	"context"

	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
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
