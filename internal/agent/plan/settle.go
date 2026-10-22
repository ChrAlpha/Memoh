package plan

import (
	"context"

	"github.com/felinics/memoh/internal/agent/runreq"
	"github.com/felinics/memoh/internal/agent/runtime/native"
)

// settleResolved does what a resolved turn owes before it runs, in the order
// resolution used to do it: the session's model preference is written back, the
// caller is told it is settled, and the channel that carries input into the
// running turn is connected.
func (p *Planner) settleResolved(ctx context.Context, req runreq.ChatRequest, rc *runreq.ResolvedContext) {
	// Persist explicit selections before generation and advance the revision
	// even for matching values. Schedule payloads describe the scheduled turn,
	// not a session preference, and never set PairCarried.
	p.writeBackModelPreference(ctx, req.ThreadID, rc.PairCarried, rc.Model, rc.RunConfig.ReasoningConfig)
	if req.OnModelPreferenceSettled != nil {
		req.OnModelPreferenceSettled()
	}
	p.connectInject(ctx, req, rc)
}

// connectInject forwards the messages injected into a running turn to the model
// runtime.
func (p *Planner) connectInject(ctx context.Context, req runreq.ChatRequest, rc *runreq.ResolvedContext) {
	if req.InjectCh == nil {
		return
	}
	runCfg := &rc.RunConfig
	supportsImages := runCfg.SupportsImageInput
	agentInjectCh := make(chan native.InjectMessage, cap(req.InjectCh))
	go func() {
		for msg := range req.InjectCh {
			agentMsg := native.InjectMessage{
				Text:            msg.Text,
				HeaderifiedText: msg.HeaderifiedText,
			}
			// Inline any image attachments from the injected message so the
			// model receives them as vision input alongside the text.
			if supportsImages && len(msg.Attachments) > 0 {
				agentMsg.ImageParts = p.runconfig.InlineInjectAttachments(ctx, req.BotID, msg.Attachments)
			}
			agentInjectCh <- agentMsg
		}
		close(agentInjectCh)
	}()
	runCfg.InjectCh = agentInjectCh
}
