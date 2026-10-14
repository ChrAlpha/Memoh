package wecom

import (
	"context"
	"errors"
	"strings"

	"github.com/felinics/memoh/internal/channel"
	"github.com/felinics/memoh/internal/redact"
)

// OpenPreview opens the stream message that shows a reply while it is
// produced. WeCom refreshes one message under one stream id, so the preview
// keeps the text so far and sends it whole on every refresh.
func (a *WeComAdapter) OpenPreview(ctx context.Context, cfg channel.ChannelConfig, target string, opts channel.StreamOptions) (channel.PreparedPreview, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("wecom target is required")
	}
	reply := opts.Reply
	if reply == nil && strings.TrimSpace(opts.SourceMessageID) != "" {
		reply = &channel.ReplyRef{
			Target:    target,
			MessageID: strings.TrimSpace(opts.SourceMessageID),
		}
	}
	return &wecomPreview{
		adapter: a,
		cfg:     cfg,
		target:  target,
		reply:   reply,
	}, nil
}

// wecomPreview is driven from one goroutine, by the manager's stream.
type wecomPreview struct {
	adapter *WeComAdapter
	cfg     channel.ChannelConfig
	target  string
	reply   *channel.ReplyRef

	text strings.Builder
	// streamID names the stream message once it exists on the platform.
	streamID string
	// shown is the last text sent to the stream.
	shown string
	ended bool
}

func (p *wecomPreview) Update(ctx context.Context, event channel.PreparedStreamEvent) error {
	if p.ended {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	switch event.Type {
	case channel.StreamEventDelta:
		if strings.TrimSpace(event.Delta) == "" || event.Phase == channel.StreamPhaseReasoning {
			return nil
		}
		p.text.WriteString(event.Delta)
		return p.refresh(ctx)
	case channel.StreamEventReset:
		// The next refresh replaces the failed attempt's text on the same stream.
		p.text.Reset()
	case channel.StreamEventToolCallEnd:
		return p.sendToolCallSummary(ctx, event.ToolCall)
	case channel.StreamEventError:
		return p.fail(ctx, event)
	}
	return nil
}

// Finish ends the stream message with the final's text. The platform cannot
// take back text it has shown, so a reply that ends without a message of its
// own closes the stream with what it shows.
func (p *wecomPreview) Finish(ctx context.Context, final *channel.PreparedStreamEvent) error {
	if p.ended {
		return nil
	}
	p.ended = true
	var msg channel.PreparedMessage
	if final != nil && final.Final != nil {
		msg = final.Final.Message
	}
	if strings.TrimSpace(msg.Message.Text) == "" {
		msg.Message.Text = p.current()
	}
	return p.deliver(ctx, msg)
}

// Abort closes the stream message with the text it already shows, so it stops
// refreshing.
func (p *wecomPreview) Abort(ctx context.Context) error {
	if p.ended {
		return nil
	}
	p.ended = true
	return p.deliver(ctx, channel.PreparedMessage{Message: channel.Message{Text: p.current()}})
}

// fail ends the reply with the error in place of the answer.
func (p *wecomPreview) fail(ctx context.Context, event channel.PreparedStreamEvent) error {
	text := redact.Text(strings.TrimSpace(event.Error))
	if text == "" {
		return nil
	}
	p.ended = true
	return p.deliver(ctx, channel.PreparedMessage{
		Message: channel.Message{Format: channel.MessageFormatPlain, Text: channel.ErrorReplyText(event.ErrorCode, text)},
	})
}

// refresh sends the text so far to the stream. Without a callback to answer,
// nothing can be refreshed and the text waits for Finish.
func (p *wecomPreview) refresh(ctx context.Context) error {
	text := strings.TrimSpace(p.text.String())
	if text == "" || text == p.shown {
		return nil
	}
	meta, ok := p.adapter.lookupCallbackContext(p.cfg.ID, p.reply)
	if !ok {
		return nil
	}
	msg := channel.PreparedMessage{Message: channel.Message{Text: text}}
	if err := p.adapter.sendRespondStream(ctx, p.cfg, msg, meta.ReqID, p.stream(), false); err != nil {
		return err
	}
	p.shown = text
	return nil
}

// deliver sends msg as the end of the reply: the last refresh of the stream
// message, or a pushed message when there is no callback to answer. A message
// with nothing in it sends nothing.
func (p *wecomPreview) deliver(ctx context.Context, msg channel.PreparedMessage) error {
	if msg.LogicalMessage().IsEmpty() {
		return nil
	}
	if msg.Message.Reply == nil {
		msg.Message.Reply = p.reply
	}
	if meta, ok := p.adapter.lookupCallbackContext(p.cfg.ID, msg.Message.Reply); ok {
		return p.adapter.sendRespondStream(ctx, p.cfg, msg, meta.ReqID, p.stream(), true)
	}
	return p.adapter.Send(ctx, p.cfg, channel.PreparedOutboundMessage{Target: p.target, Message: msg})
}

// current is the text the reply shows now: what has accumulated since the last
// reset, else what the stream still shows from before it.
func (p *wecomPreview) current() string {
	if text := strings.TrimSpace(p.text.String()); text != "" {
		return text
	}
	return p.shown
}

func (p *wecomPreview) stream() string {
	if p.streamID == "" {
		p.streamID = NewReqID("stream")
	}
	return p.streamID
}

// sendToolCallSummary pushes the terminal state of a tool call as a message of
// its own. A stream message cannot carry it, and the running state is left out
// to avoid duplicate messages.
func (p *wecomPreview) sendToolCallSummary(ctx context.Context, tc *channel.StreamToolCall) error {
	text := strings.TrimSpace(channel.RenderToolCallMessage(channel.BuildToolCallEnd(tc)))
	if text == "" {
		return nil
	}
	return p.adapter.Send(ctx, p.cfg, channel.PreparedOutboundMessage{
		Target: p.target,
		Message: channel.PreparedMessage{
			Message: channel.Message{Format: channel.MessageFormatPlain, Text: text},
		},
	})
}
