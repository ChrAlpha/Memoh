package native

import (
	"context"
	"fmt"
	"slices"
	"strings"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// ContextSwitchInput asks for one summary of the run's committed steps before
// LatestStep, which the next request keeps verbatim with everything before
// the run's first step.
type ContextSwitchInput struct {
	LatestStep      int
	KeptTokens      int
	AllowanceTokens int
}

// contextSwitchState follows, in the thread a dispatch advances, where the
// block of the latest committed step starts: the inputs drained at its
// boundary, hook context, and the step's own messages. Between the request
// prefix and that block lie only earlier committed steps of the run or the
// summary of them, which is what a context switch replaces.
type contextSwitchState struct {
	latest     int
	latestStep int
	pending    int
	steps      int
	restarted  bool
}

// restart begins a dispatch. A re-dispatch resumes a call whose inputs are
// already in its request, so where its block starts is unknown.
func (s *contextSwitchState) restart(prefix int) {
	if s == nil {
		return
	}
	s.latest, s.steps, s.pending = -1, 0, prefix
	if s.restarted {
		s.pending = -1
	}
	s.restarted = true
}

func (s *contextSwitchState) boundary(at int) {
	if s != nil {
		s.pending = at
	}
}

func (s *contextSwitchState) committed(step int) {
	if s == nil {
		return
	}
	s.latest, s.latestStep, s.pending = s.pending, step, -1
	s.steps++
}

func (s *contextSwitchState) invalidate() {
	if s != nil {
		s.latest, s.pending, s.steps = -1, -1, 0
	}
}

// fold replaces the earlier committed steps with the summary SwitchContext
// writes for them when the request does not fit. It reports how many
// messages the request lost; a summary that leaves the request too large
// fails the attempt.
func (s *contextSwitchState) fold(ctx context.Context, cfg RunConfig, params *sdk.Request, prefix, allowance int) (int, error) {
	if s == nil || cfg.SwitchContext == nil || allowance <= 0 || s.steps < 2 || s.latest <= prefix || s.latest > len(params.Messages) {
		return 0, nil
	}
	before := contextfrag.ProviderEnvelopeTokens(params.System, params.Messages, params.Tools)
	if before <= allowance {
		return 0, nil
	}
	kept := slices.Concat(params.Messages[:prefix], params.Messages[s.latest:])
	summary, err := cfg.SwitchContext(ctx, ContextSwitchInput{
		LatestStep:      s.latestStep,
		KeptTokens:      contextfrag.ProviderEnvelopeTokens(params.System, kept, params.Tools),
		AllowanceTokens: allowance,
	})
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return 0, err
	}
	if !slices.ContainsFunc(summary.Content, func(part sdk.MessagePart) bool {
		text, ok := part.(sdk.TextPart)
		return ok && strings.TrimSpace(text.Text) != ""
	}) {
		return 0, fmt.Errorf("%w: context switch returned no summary", contextfrag.ErrBudgetUnsatisfied)
	}
	messages := slices.Concat(params.Messages[:prefix], []sdk.Message{summary}, params.Messages[s.latest:])
	if after := contextfrag.ProviderEnvelopeTokens(params.System, messages, params.Tools); after >= before || after > allowance {
		return 0, fmt.Errorf("%w: context switch left %d of %d input tokens, allowance %d", contextfrag.ErrBudgetUnsatisfied, after, before, allowance)
	}
	params.Messages = messages
	dropped := s.latest - prefix - 1
	s.latest, s.steps = prefix+1, 1
	if s.pending >= 0 {
		s.pending -= dropped
	}
	return dropped, nil
}
