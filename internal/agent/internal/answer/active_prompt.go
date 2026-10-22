package answer

import (
	"context"
	"strings"

	"github.com/felinics/memoh/internal/agent/internal/activeprompt"
	"github.com/felinics/memoh/internal/agent/internal/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

type externalAgentActivePromptForwardOptions struct {
	SkipToolCallID  string
	SkipUserInputID string
	SkipApprovalID  string
}

func forwardExternalAgentActivePrompt(ctx context.Context, sub *activeprompt.Subscription, eventCh chan<- runreq.WSStreamEvent, opts externalAgentActivePromptForwardOptions) error {
	if sub == nil {
		return runreq.EmitApprovalAck(ctx, eventCh)
	}
	defer sub.Release()
	if eventCh == nil {
		return nil
	}
	if err := runreq.SendAgentStreamEvent(ctx, eventCh, native.StreamEvent{Type: native.EventStart}); err != nil {
		return err
	}
	for {
		ev, ok, err := sub.Next(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return runreq.SendAgentStreamEvent(ctx, eventCh, native.StreamEvent{Type: native.EventAbort})
		}
		if opts.skip(ev) {
			continue
		}
		if err := runreq.SendAgentStreamEvent(ctx, eventCh, ev); err != nil {
			return err
		}
		if ev.IsTerminal() {
			return nil
		}
	}
}

func (o externalAgentActivePromptForwardOptions) skip(ev native.StreamEvent) bool {
	switch ev.Type {
	case native.EventUserInputRequest:
		if sameNonEmpty(ev.UserInputID, o.SkipUserInputID) {
			return true
		}
		return sameNonEmpty(ev.ToolCallID, o.SkipToolCallID)
	case native.EventToolApprovalRequest:
		if sameNonEmpty(ev.ApprovalID, o.SkipApprovalID) {
			return true
		}
		return sameNonEmpty(ev.ToolCallID, o.SkipToolCallID)
	case native.EventToolCallInputStart,
		native.EventToolCallStart,
		native.EventToolCallMetadata,
		native.EventToolCallProgress,
		native.EventToolCallEnd:
		return sameNonEmpty(ev.ToolCallID, o.SkipToolCallID)
	default:
		return false
	}
}

func sameNonEmpty(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	return a != "" && b != "" && a == b
}
