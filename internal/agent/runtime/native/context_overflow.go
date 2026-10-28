package native

import (
	"context"
	"fmt"
	"log/slog"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/models/modelretry"
)

const maxContextRecoveries = 2

// recoverContextOverflow runs at the failed model step, before any output
// from that attempt commits or any of its tools execute. It uses a smaller
// allowance to force the existing durable fold even when the local estimate
// passed admission. A new dispatch is permitted only after an applied pass
// actually reduces the rejected request.
func (a *Agent) recoverContextOverflow(ctx context.Context, cfg RunConfig, request *sdk.Request, failure error, recoveries *int) (bool, error) {
	feedback, overflow := modelretry.ContextOverflow(failure)
	if !overflow || cfg.contextSwitch == nil {
		return false, failure
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if *recoveries >= maxContextRecoveries {
		return false, fmt.Errorf("%w: provider context recovery exhausted: %w", contextfrag.ErrBudgetUnsatisfied, failure)
	}
	before := contextfrag.ProviderEnvelopeTokens(request.System, request.Messages, request.Tools)
	allowance := feedback.InputBudget(min(before, providerInputAllowance(cfg)))
	changed, err := cfg.contextSwitch.fold(ctx, cfg, request, allowance)
	if err != nil {
		return false, err
	}
	after := contextfrag.ProviderEnvelopeTokens(request.System, request.Messages, request.Tools)
	if !changed || after >= before || after > allowance {
		return false, fmt.Errorf("%w: provider context recovery made no progress: %w", contextfrag.ErrBudgetUnsatisfied, failure)
	}
	*recoveries++
	a.logger.WarnContext(ctx, "provider context overflow recovered",
		slog.String("run_id", cfg.RunID), slog.Int("attempt", *recoveries),
		slog.Int("estimated_input_before", before), slog.Int("estimated_input_after", after),
		slog.Int("input_allowance", allowance), slog.Int("provider_input_tokens", feedback.InputTokens), slog.Int("provider_limit_tokens", feedback.LimitTokens))
	return true, nil
}
