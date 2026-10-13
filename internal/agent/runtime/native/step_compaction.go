package native

import (
	"context"

	sdk "github.com/felinics/twilight/sdk"

	contextfrag "github.com/felinics/memoh/internal/agent/context/fragment"
)

// StepContextInput is what the loop knows about the request it is about to
// send: the part of it no conversation replacement can shrink, and the
// conversation. Together they are the whole provider payload.
type StepContextInput struct {
	// FixedTokens is the estimated system prompt and tool definitions,
	// wherever the prompt-cache plan put the system prompt.
	FixedTokens int
	// Messages is the conversation the loop holds, without a system prompt
	// the prompt-cache plan moved into it. A compactor must not modify it.
	// It is what lets a run whose own steps are not readable from storage
	// carry them over as they are.
	Messages []sdk.Message
}

// StepContextCompactor lets the caller shrink a run's context before each of
// its model calls. It returns the conversation to continue with, without the
// system prompt, or nil to keep the current one. The loop asks before every
// call, the run's first included, ahead of the boundary's own inputs, so the
// messages it holds are the compiled request and the ones its committed steps
// produced. A retried dispatch resends the failed attempt's request without
// asking again.
type StepContextCompactor func(ctx context.Context, input StepContextInput) []sdk.Message

// compactContext offers the conversation to the run's compactor and, when it
// hands one back, continues on it. The replacement is protected prefix: the
// loop owns nothing in it, and the cached prefix of the old conversation is
// gone with it.
func (t *stepThread) compactContext(ctx context.Context, cfg RunConfig) {
	if cfg.StepContextCompactor == nil {
		return
	}
	conversation, system := t.messages, []sdk.Message(nil)
	if t.dispatch.systemPrepended && len(conversation) > 0 {
		conversation, system = conversation[1:], conversation[:1]
	}
	replacement := cfg.StepContextCompactor(ctx, StepContextInput{
		FixedTokens: contextfrag.ProviderEnvelopeTokens(t.params.System, system, t.params.Tools),
		Messages:    conversation,
	})
	if replacement == nil {
		return
	}
	messages := make([]sdk.Message, 0, len(replacement)+1)
	if t.dispatch.systemPrepended && len(t.messages) > 0 {
		messages = append(messages, t.messages[0])
	}
	messages = append(messages, replacement...)
	t.messages = messages
	t.params.Messages = messages
	t.dispatch.prefix.count = len(messages)
}
