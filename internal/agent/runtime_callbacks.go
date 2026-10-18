package agent

import (
	"context"
	"log/slog"

	"github.com/felinics/memoh/internal/agent/outcome"
	sessionruntime "github.com/felinics/memoh/internal/agent/runtime/session"
)

// bindSessionRuntime registers the callbacks the runtime invokes into the
// service's components. The manager is built before the service and holds no
// reference back to it, so New can wire both ways.
func bindSessionRuntime(manager *sessionruntime.Manager, logger *slog.Logger, s *Service) {
	manager.SetDecisionStore(s.decider)
	manager.SetLostRunDecisionCanceller(s.decider.CancelLostRunDecisions)
	manager.SetCommandHandler(func(ctx context.Context, command sessionruntime.Command) error {
		return s.answer.HandleCommand(ctx, command)
	})
	manager.SetDecisionFinalizer(s.decider.FinalizeRuntimeDecisions)
	manager.SetTerminalObserver(terminalFanout(
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			outcome.LogRunResult(ctx, logger, terminal)
		},
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			s.lifecycle.ReconcileTerminal(ctx, terminal)
		},
		func(ctx context.Context, terminal sessionruntime.TerminalRun) {
			s.pending.AfterSessionTerminal(ctx, terminal)
		},
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
