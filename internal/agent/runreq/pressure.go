package runreq

import (
	"sync"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// RequestInputTokens estimates the input of a whole provider request: the
// part no compaction can shrink (system prompt, tool definitions, and
// whatever else the caller counts as fixed) plus the conversation. The
// pre-turn, between-calls and run-end decisions all measure this one number
// against the same thresholds.
func RequestInputTokens(fixedTokens int, messages []sdk.Message) int {
	return fixedTokens + contextfrag.ProviderEnvelopeTokens("", messages, nil)
}

// LifecycleFixedTokens is the system prompt and tool definitions a run's
// context plan priced.
func LifecycleFixedTokens(snapshot contextfrag.LifecycleSnapshot) int {
	if snapshot.BudgetPlan == nil {
		return 0
	}
	return snapshot.BudgetPlan.ActualSystemCost + snapshot.BudgetPlan.ToolDefsCost
}

// RunPressure is a native run's estimate of the request its next model call
// would make. Its conversation starts from what the turn assembled, is
// replaced by the loop's own at every call the run is offered, and grows by
// every step committed after that; the system prompt and tools are priced
// from the run's context plan. When the run ends it prices the context the
// session now holds.
type RunPressure struct {
	lifecycle *contextfrag.LifecycleHolder

	mu           sync.Mutex
	Conversation int
}

// NewRunPressure starts a run's request-pressure record from its lifecycle and conversation.
func NewRunPressure(lifecycle *contextfrag.LifecycleHolder, conversation []sdk.Message) *RunPressure {
	return &RunPressure{lifecycle: lifecycle, Conversation: RequestInputTokens(0, conversation)}
}

// observe takes the conversation a model call is about to send.
func (p *RunPressure) Observe(conversation []sdk.Message) {
	if p == nil {
		return
	}
	tokens := RequestInputTokens(0, conversation)
	p.mu.Lock()
	p.Conversation = tokens
	p.mu.Unlock()
}

// add counts a committed step.
func (p *RunPressure) Add(messages []sdk.Message) {
	if p == nil || len(messages) == 0 {
		return
	}
	tokens := RequestInputTokens(0, messages)
	p.mu.Lock()
	p.Conversation += tokens
	p.mu.Unlock()
}

// Tokens estimates the input tokens of the run's next request.
func (p *RunPressure) Tokens() int {
	p.mu.Lock()
	conversation := p.Conversation
	p.mu.Unlock()
	fixed := 0
	if snapshot, ok := p.lifecycle.Snapshot(); ok {
		fixed = LifecycleFixedTokens(snapshot)
	}
	return fixed + conversation
}
