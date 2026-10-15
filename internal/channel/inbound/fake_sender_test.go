package inbound

import (
	"context"

	"github.com/felinics/memoh/internal/channel"
)

// fakeSenderCaps is what the test senders declare: every event the delivery
// layer can hand over reaches their stream.
var fakeSenderCaps = channel.ChannelCapabilities{
	Text: true, Markdown: true, Streaming: true, Attachments: true, Reactions: true,
	NativeApproval: true, NativeUserInput: true, Preview: channel.PreviewEditInPlace,
}

type openStreamer interface {
	OpenStream(ctx context.Context, target string, opts channel.StreamOptions) (channel.OutboundStream, error)
}

// streamPreview is the preview of a test sender: it forwards to the sender's
// OutboundStream, the way the manager does for an adapter's stream.
type streamPreview struct{ stream channel.OutboundStream }

func openStreamPreview(ctx context.Context, sender openStreamer, target string, opts channel.StreamOptions) (channel.Preview, error) {
	stream, err := sender.OpenStream(ctx, target, opts)
	if err != nil {
		return nil, err
	}
	return &streamPreview{stream: stream}, nil
}

func (p *streamPreview) Update(ctx context.Context, event channel.StreamEvent) error {
	return p.stream.Push(ctx, event)
}

func (p *streamPreview) Finish(ctx context.Context, final *channel.StreamEvent) error {
	if final != nil {
		if err := p.stream.Push(ctx, *final); err != nil {
			_ = p.stream.Close(ctx)
			return err
		}
	}
	if err := p.stream.Push(ctx, channel.StreamEvent{Type: channel.StreamEventStatus, Status: channel.StreamStatusCompleted}); err != nil {
		_ = p.stream.Close(ctx)
		return err
	}
	return p.stream.Close(ctx)
}

func (p *streamPreview) Abort(ctx context.Context) error { return p.stream.Close(ctx) }

func (s *fakeReplySender) Capabilities(string) channel.ChannelCapabilities {
	caps := fakeSenderCaps
	if s.plainPrompts {
		caps.NativeApproval, caps.NativeUserInput = false, false
	}
	caps.ReplyWindow = s.replyWindow
	return caps
}

func (s *fakeReplySender) Attach(_ context.Context, target string, att channel.Attachment, reply *channel.ReplyRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, channel.OutboundMessage{Target: target, Message: channel.Message{Attachments: []channel.Attachment{att}, Reply: reply}})
	return nil
}

func (s *fakeReplySender) React(_ context.Context, req channel.ReactRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reactions = append(s.reactions, req)
	return nil
}

func (s *fakeReplySender) OpenPreview(ctx context.Context, target string, opts channel.StreamOptions) (channel.Preview, error) {
	return openStreamPreview(ctx, s, target, opts)
}

func (*failingOpenPreviewSender) Capabilities(string) channel.ChannelCapabilities {
	return fakeSenderCaps
}

func (*failingOpenPreviewSender) Attach(context.Context, string, channel.Attachment, *channel.ReplyRef) error {
	return nil
}

func (*failingOpenPreviewSender) React(context.Context, channel.ReactRequest) error { return nil }

func (s *failingOpenPreviewSender) OpenPreview(ctx context.Context, target string, opts channel.StreamOptions) (channel.Preview, error) {
	return openStreamPreview(ctx, s, target, opts)
}

func (*failingCloseSender) Capabilities(string) channel.ChannelCapabilities { return fakeSenderCaps }

func (*failingCloseSender) Attach(context.Context, string, channel.Attachment, *channel.ReplyRef) error {
	return nil
}

func (*failingCloseSender) React(context.Context, channel.ReactRequest) error { return nil }

func (s *failingCloseSender) OpenPreview(ctx context.Context, target string, opts channel.StreamOptions) (channel.Preview, error) {
	return openStreamPreview(ctx, s, target, opts)
}
