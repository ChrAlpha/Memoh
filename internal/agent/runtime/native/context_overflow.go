package native

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
	"github.com/felinics/memoh/internal/errlog"
	"github.com/felinics/memoh/internal/models/modelretry"
)

const maxContextRecoveries = 2

// Some providers finish with length and zero output when input consumes the
// window instead of returning an API error. Measured input above our allowance
// makes that an unfinished step. Ordinary output truncation and any generated
// content remain completed work and must not be replayed.
func exhaustedContextResult(cfg RunConfig, result sdk.ModelResult) error {
	if result.FinishReason != sdk.FinishReasonLength || result.Usage.OutputTokens != 0 || result.Usage.ReasoningTokens != 0 ||
		result.Text != "" || result.Reasoning != "" || len(result.ReasoningParts) != 0 ||
		len(result.ToolCalls) != 0 || len(result.Files) != 0 || len(result.Sources) != 0 {
		return nil
	}
	allowance := providerInputAllowance(cfg)
	if allowance <= 0 || result.Usage.InputTokens <= allowance {
		return nil
	}
	return &emptyContextExhaustion{inputTokens: result.Usage.InputTokens}
}

type emptyContextExhaustion struct{ inputTokens int }

func (*emptyContextExhaustion) Error() string {
	return "model exhausted context without producing output"
}

func (*emptyContextExhaustion) Unwrap() error { return contextfrag.ErrBudgetUnsatisfied }

func contextRecoveryFeedback(err error) (modelretry.Overflow, bool) {
	var exhausted *emptyContextExhaustion
	if errors.As(err, &exhausted) {
		// Usage reports the consumed input, not the provider's hard limit.
		return modelretry.Overflow{InputTokens: exhausted.inputTokens}, true
	}
	return modelretry.ContextOverflow(err)
}

// recoverContextOverflow runs at the failed model step, before any output
// from that attempt commits or any of its tools execute. It uses a smaller
// allowance to force the existing durable fold even when the local estimate
// passed admission. A new dispatch is permitted only after an applied pass
// actually reduces the rejected request.
func (a *Agent) recoverContextOverflow(ctx context.Context, cfg RunConfig, request *sdk.Request, failure error, recoveries *int) (bool, error) {
	feedback, overflow := contextRecoveryFeedback(failure)
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
	// fold accepts an admission allowance and converts it once into the
	// planner's raw estimate. Tightening the raw estimate here would apply
	// that conversion twice when a matching usage measurement is present.
	allowance := feedback.InputBudget(min(requestInputTokens(cfg, *request), providerInputAllowance(cfg)))
	changed, err := cfg.contextSwitch.fold(ctx, cfg, request, allowance)
	if err != nil {
		return false, err
	}
	after := contextfrag.ProviderEnvelopeTokens(request.System, request.Messages, request.Tools)
	if !changed || after >= before || requestInputTokens(cfg, *request) > allowance {
		return false, fmt.Errorf("%w: provider context recovery made no progress: %w", contextfrag.ErrBudgetUnsatisfied, failure)
	}
	*recoveries++
	event := errlog.Event(ctx, "agent.context_recovery", failure, errlog.Options{})
	a.logger.LogAttrs(ctx, event.Level, "provider context overflow recovered", append([]slog.Attr{
		slog.String("run_id", cfg.RunID), slog.Int("attempt", *recoveries),
		slog.Int("estimated_input_before", before), slog.Int("estimated_input_after", after),
		slog.Int("input_allowance", allowance), slog.Int("provider_input_tokens", feedback.InputTokens), slog.Int("provider_limit_tokens", feedback.LimitTokens),
	}, event.Attrs()...)...)
	return true, nil
}
